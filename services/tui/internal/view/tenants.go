package view

import (
	"github.com/charmbracelet/lipgloss"

	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
	"github.com/caelum0x/ration/services/tui/internal/format"
)

var tenantHeaders = []string{"NAME", "CCY", "RESERVE FLOOR", "AVAILABLE", "HELD", "HEADROOM", "STATUS", "CREATED"}
var tenantWidths = []int{18, 5, 14, 14, 12, 14, 7, 19}

// Tenants renders the tenants tab: one row per tenant with its reserve
// floor, current ledger balance and the headroom actually available for
// bridge funding (available - reserve floor), the number that drives every
// rationing decision in docs/AGENT-DESIGN.md.
func Tenants(tenants []*ledgerpb.Tenant, balances map[string]*ledgerpb.Balance, cursor int) string {
	rows := make([][]string, 0, len(tenants))
	for _, t := range tenants {
		bal := balances[t.GetId()]
		available := int64(0)
		held := int64(0)
		if bal != nil {
			available = bal.GetAvailableMinorUnits()
			held = bal.GetHeldMinorUnits()
		}
		headroom := available - t.GetReserveFloorMinorUnits()
		status := "OK"
		if headroom < 0 {
			status = "BREACH"
		}
		rows = append(rows, []string{
			// t.GetName() is operator-supplied free text with no
			// server-side charset validation (services/tenant-registry/
			// internal/tenant.Service.Create only checks non-emptiness) —
			// same unsanitized-free-text-into-terminal class rounds
			// 13/16/23 already closed for CardholderName/the CLI's own
			// tenant-create echo/the Activity feed's Summary, but never
			// for a Tenant's own Name as rendered by this tab. Sanitize
			// via the same helper decisions.go/cards.go already use.
			format.SanitizedCell(t.GetName(), tenantWidths[0]),
			t.GetCurrency(),
			format.Money(t.GetReserveFloorMinorUnits(), t.GetCurrency()),
			format.Money(available, t.GetCurrency()),
			format.Money(held, t.GetCurrency()),
			format.Money(headroom, t.GetCurrency()),
			status,
			format.Unix(t.GetCreatedAtUnix()),
		})
	}

	return Table(tenantHeaders, rows, tenantWidths, cursor, 6, tenantStatusStyle)
}

func tenantStatusStyle(label string) lipgloss.Style {
	if label == "BREACH" {
		return lipgloss.NewStyle().Foreground(colorBad).Bold(true)
	}
	return lipgloss.NewStyle().Foreground(colorGood)
}
