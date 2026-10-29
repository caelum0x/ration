package model

import (
	"context"
	"fmt"
	"strconv"

	tea "github.com/charmbracelet/bubbletea"

	gatewaypb "github.com/caelum0x/ration/pkg/pb/gateway"
	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
	"github.com/caelum0x/ration/services/tui/internal/client"
)

// cardFormKind distinguishes the TUI's two interactive card actions —
// everything else about a form (field navigation, text editing,
// submit/cancel) is shared.
type cardFormKind int

const (
	cardFormIssue cardFormKind = iota
	cardFormAdjustLimit
)

// formFieldKind picks how a field accepts input: free text, digits
// only, or a left/right-cycled set of options.
type formFieldKind int

const (
	fieldText formFieldKind = iota
	fieldNumeric
	fieldPicker
)

// formField is one editable row of a cardForm. value backs
// fieldText/fieldNumeric; options/optIdx back fieldPicker.
type formField struct {
	label   string
	kind    formFieldKind
	value   []rune
	options []string
	optIdx  int
}

func (f *formField) text() string {
	if f.kind == fieldPicker {
		if f.optIdx < 0 || f.optIdx >= len(f.options) {
			return ""
		}
		return f.options[f.optIdx]
	}
	return string(f.value)
}

// cardForm is the TUI's one interactive input surface: a small modal
// over the Cards tab that lets an operator actually call
// gateway.IssueCard / gateway.AdjustCardLimit instead of only ever
// observing cards the agent's own decisions created. Before this
// existed, PLAN-2026-10-23-DEEP.md's own "Remaining" note for the
// Cards tab was still true: "no in-TUI IssueCard/AdjustCardLimit form
// yet (CLI/API only)" — round 7 closed the CLI half of that gap, this
// closes the TUI half.
type cardForm struct {
	kind       cardFormKind
	fields     []*formField
	focus      int
	tenantIDs  []string // issue-only, parallel to fields[0].options
	cardID     string   // adjust-only
	infoLine   string   // adjust-only: read-only "tenant — cardholder — current limit" header
	submitting bool
	err        string
}

// newIssueCardForm builds the "issue card" form, defaulting the tenant
// picker to selectedIdx (the row under the cursor on whichever tab the
// operator triggered this from, clamped by the caller).
func newIssueCardForm(tenants []*ledgerpb.Tenant, selectedIdx int) *cardForm {
	names := make([]string, len(tenants))
	ids := make([]string, len(tenants))
	for i, t := range tenants {
		names[i] = fmt.Sprintf("%s (%s)", t.GetName(), t.GetCurrency())
		ids[i] = t.GetId()
	}
	if selectedIdx < 0 || selectedIdx >= len(tenants) {
		selectedIdx = 0
	}
	return &cardForm{
		kind:      cardFormIssue,
		tenantIDs: ids,
		fields: []*formField{
			{label: "Tenant", kind: fieldPicker, options: names, optIdx: selectedIdx},
			{label: "Cardholder", kind: fieldText},
			{label: "Limit (minor units)", kind: fieldNumeric},
			{label: "Interval", kind: fieldPicker, options: []string{"monthly", "daily"}},
			{label: "Currency (blank = tenant default)", kind: fieldText},
		},
	}
}

// newAdjustLimitForm builds the "adjust limit" form for an already-
// selected card, pre-filling the new-limit field with its current
// limit (edit in place, not from scratch) and a sensible default
// reason.
func newAdjustLimitForm(card *gatewaypb.Card, tenantName string) *cardForm {
	return &cardForm{
		kind:     cardFormAdjustLimit,
		cardID:   card.GetId(),
		infoLine: fmt.Sprintf("%s — %s — current limit %s", tenantName, card.GetCardholderName(), card.GetLimitInterval()),
		fields: []*formField{
			{label: "New limit (minor units)", kind: fieldNumeric, value: []rune(strconv.FormatInt(card.GetLimitMinorUnits(), 10))},
			{label: "Reason", kind: fieldText, value: []rune("ration tui adjust-limit")},
		},
	}
}

// handleKey mutates the form in place for every key except submit
// (enter) and cancel (esc), which the caller (Model.handleKey) still
// decides what to do with — this keeps cardForm itself free of any
// gRPC/tea.Cmd concerns beyond building the one request it validates.
func (f *cardForm) handleKey(msg tea.KeyMsg) {
	if f.submitting {
		return
	}
	cur := f.fields[f.focus]

	switch msg.String() {
	case "tab", "down":
		f.focus = (f.focus + 1) % len(f.fields)
	case "shift+tab", "up":
		f.focus = (f.focus - 1 + len(f.fields)) % len(f.fields)
	case "left":
		if cur.kind == fieldPicker && len(cur.options) > 0 {
			cur.optIdx = (cur.optIdx - 1 + len(cur.options)) % len(cur.options)
		}
	case "right":
		if cur.kind == fieldPicker && len(cur.options) > 0 {
			cur.optIdx = (cur.optIdx + 1) % len(cur.options)
		}
	case "backspace":
		if cur.kind != fieldPicker && len(cur.value) > 0 {
			cur.value = cur.value[:len(cur.value)-1]
		}
	default:
		// msg.Runes can carry more than one rune in a single KeyMsg —
		// bubbletea's input reader batches consecutive printable bytes
		// read in one syscall rather than guaranteeing one KeyMsg per
		// keystroke (observed live: typing/pasting a multi-character
		// name arrived as one KeyMsg here, not N single-rune ones) — so
		// every rune in the batch is appended, not just msg.Runes[0].
		if cur.kind == fieldPicker || len(msg.Runes) == 0 {
			return
		}
		for _, r := range msg.Runes {
			if cur.kind == fieldNumeric && (r < '0' || r > '9') {
				continue
			}
			if r >= 0x20 { // no control characters — see pkg/textsanitize's stated rationale
				cur.value = append(cur.value, r)
			}
		}
	}
}

// validate checks the form has everything needed to submit, without
// making any RPC — called before buildIssueCardRequest/
// buildAdjustLimitRequest so a bad input shows a message instead of a
// gRPC error for something that was never going to be well-formed.
func (f *cardForm) validate() error {
	switch f.kind {
	case cardFormIssue:
		if f.fields[1].text() == "" {
			return fmt.Errorf("cardholder is required")
		}
		if f.fields[2].text() == "" {
			return fmt.Errorf("limit is required (0 issues an unfunded card)")
		}
	case cardFormAdjustLimit:
		if f.fields[0].text() == "" {
			return fmt.Errorf("new limit is required")
		}
	}
	return nil
}

func (f *cardForm) issueCardRequest() *gatewaypb.IssueCardRequest {
	limit, _ := strconv.ParseInt(f.fields[2].text(), 10, 64)
	tenantIdx := f.fields[0].optIdx
	tenantID := ""
	if tenantIdx >= 0 && tenantIdx < len(f.tenantIDs) {
		tenantID = f.tenantIDs[tenantIdx]
	}
	return &gatewaypb.IssueCardRequest{
		TenantId:        tenantID,
		CardholderName:  f.fields[1].text(),
		LimitMinorUnits: limit,
		LimitInterval:   f.fields[3].text(),
		Currency:        f.fields[4].text(),
	}
}

func (f *cardForm) adjustCardLimitRequest() *gatewaypb.AdjustCardLimitRequest {
	limit, _ := strconv.ParseInt(f.fields[0].text(), 10, 64)
	return &gatewaypb.AdjustCardLimitRequest{
		CardId:             f.cardID,
		NewLimitMinorUnits: limit,
		Reason:             f.fields[1].text(),
	}
}

// formResultMsg carries the outcome of an IssueCard/AdjustCardLimit
// RPC back into Model.Update. On success the form closes and an
// immediate refresh is triggered (not waiting for the next 3s tick) so
// the operator sees the exact card they just created/changed right
// away. On failure the form stays open with err set so they can fix
// input and retry without re-entering everything.
type formResultMsg struct {
	err error
}

// submitForm fires the one gRPC call a cardForm represents. Validation
// already happened in Model.handleKey before this is ever scheduled.
func submitForm(ctx context.Context, c *client.Clients, f *cardForm) tea.Cmd {
	return func() tea.Msg {
		rpcCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
		defer cancel()

		var err error
		switch f.kind {
		case cardFormIssue:
			_, err = c.IssueCard(rpcCtx, f.issueCardRequest())
		case cardFormAdjustLimit:
			_, err = c.AdjustCardLimit(rpcCtx, f.adjustCardLimitRequest())
		}
		return formResultMsg{err: err}
	}
}
