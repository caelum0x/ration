// Package model holds the top-level Bubble Tea model for the Ration TUI:
// tab state, fetched data, the live activity feed, and the Update loop
// that ties polling + streaming + key handling together. Rendering itself
// is delegated to internal/view, which knows nothing about gRPC or state.
package model

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	gatewaypb "github.com/caelum0x/ration/pkg/pb/gateway"
	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
	"github.com/caelum0x/ration/services/tui/internal/client"
	"github.com/caelum0x/ration/services/tui/internal/view"
)

// refreshInterval is how often tenants/deposits/cards/balances are
// re-polled from the gateway and ledger-core. The activity feed is
// independently live via StreamActivity, not on this timer.
const refreshInterval = 3 * time.Second

// fetchTimeout bounds a single polling refresh so a stalled gRPC call
// can't wedge the tick loop forever.
const fetchTimeout = 4 * time.Second

// decisionsPerTenant bounds how many of each tenant's most recent
// agent decisions the Decisions tab fetches per refresh — this is a
// live dashboard, not an audit export (see `ration status`'s CLI
// equivalent, or `ration audit-check` for a full count comparison), so
// a handful of the newest decisions per tenant is what the judge/
// operator actually wants to see scroll by.
const decisionsPerTenant = 20

// refreshMsg carries the result of one polling cycle across tenants,
// deposits, cards and balances. err is set (data left as-is) when the
// ledger-core/gateway call itself fails, e.g. the service isn't up yet.
type refreshMsg struct {
	tenants   []*ledgerpb.Tenant
	deposits  []*ledgerpb.Deposit
	cards     []*gatewaypb.Card
	balances  map[string]*ledgerpb.Balance
	decisions []*ledgerpb.AgentDecision
	err       error
}

type tickMsg time.Time

// Model is the top-level tea.Model for the dashboard.
type Model struct {
	clients *client.Clients
	ctx     context.Context
	cancel  context.CancelFunc
	events  chan tea.Msg

	activeTab Tab
	cursor    [tabCount]int

	tenants     []*ledgerpb.Tenant
	tenantNames map[string]string
	deposits    []*ledgerpb.Deposit
	cards       []*gatewaypb.Card
	balances    map[string]*ledgerpb.Balance
	decisions   []*ledgerpb.AgentDecision

	activity []view.ActivityRow

	width, height int
	err           error
	connected     bool
	lastRefresh   time.Time
}

// New builds the initial model wired to the given (already dialed) gRPC
// clients. The returned model owns a background context that is canceled
// when the program quits, stopping the activity-stream goroutine.
func New(clients *client.Clients) Model {
	ctx, cancel := context.WithCancel(context.Background())
	return Model{
		clients:     clients,
		ctx:         ctx,
		cancel:      cancel,
		events:      make(chan tea.Msg, 128),
		activeTab:   TabTenants,
		tenantNames: map[string]string{},
		balances:    map[string]*ledgerpb.Balance{},
	}
}

// Init kicks off the activity stream listener, the first data fetch, and
// the recurring refresh tick.
func (m Model) Init() tea.Cmd {
	return tea.Batch(
		startActivityStream(m.ctx, m.clients, m.events),
		waitForActivity(m.events),
		fetchAll(m.ctx, m.clients),
		tickEvery(refreshInterval),
	)
}

// Update implements tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case refreshMsg:
		m.applyRefresh(msg)
		return m, nil

	case tickMsg:
		return m, tea.Batch(fetchAll(m.ctx, m.clients), tickEvery(refreshInterval))

	case activityMsg:
		row := view.ActivityRow{
			ID:         msg.id,
			TenantName: resolveTenantName(m.tenantNames, msg.tenantID),
			Kind:       msg.kind,
			Summary:    msg.summary,
			AtUnix:     msg.atUnix,
		}
		m.activity = prependActivity(m.activity, row, maxActivityRows)
		m.cursor[TabActivity] = clampCursor(m.cursor[TabActivity], len(m.activity))
		return m, waitForActivity(m.events)

	case streamStatusMsg:
		m.connected = msg.connected
		if msg.err != nil {
			m.err = msg.err
		}
		return m, waitForActivity(m.events)
	}
	return m, nil
}

func (m *Model) applyRefresh(msg refreshMsg) {
	if msg.err != nil {
		m.err = msg.err
		return
	}
	m.err = nil
	m.tenants = msg.tenants
	m.tenantNames = buildTenantNames(msg.tenants)
	m.deposits = msg.deposits
	m.cards = msg.cards
	m.balances = msg.balances
	m.decisions = msg.decisions
	m.lastRefresh = time.Now()

	m.cursor[TabTenants] = clampCursor(m.cursor[TabTenants], len(m.tenants))
	m.cursor[TabDeposits] = clampCursor(m.cursor[TabDeposits], len(m.deposits))
	m.cursor[TabCards] = clampCursor(m.cursor[TabCards], len(m.cards))
	m.cursor[TabDecisions] = clampCursor(m.cursor[TabDecisions], len(m.decisions))
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "q":
		m.cancel()
		return m, tea.Quit

	case "tab", "right", "l":
		m.activeTab = NextTab(m.activeTab)
		return m, nil

	case "shift+tab", "left", "h":
		m.activeTab = PrevTab(m.activeTab)
		return m, nil

	case "1":
		m.activeTab = TabTenants
		return m, nil
	case "2":
		m.activeTab = TabDeposits
		return m, nil
	case "3":
		m.activeTab = TabCards
		return m, nil
	case "4":
		m.activeTab = TabDecisions
		return m, nil
	case "5":
		m.activeTab = TabActivity
		return m, nil

	case "up", "k":
		m.cursor[m.activeTab] = clampCursor(m.cursor[m.activeTab]-1, m.rowCount(m.activeTab))
		return m, nil

	case "down", "j":
		m.cursor[m.activeTab] = clampCursor(m.cursor[m.activeTab]+1, m.rowCount(m.activeTab))
		return m, nil

	case "r":
		return m, fetchAll(m.ctx, m.clients)
	}
	return m, nil
}

// rowCount returns how many rows the given tab currently has, used to
// clamp its cursor on up/down.
func (m Model) rowCount(t Tab) int {
	switch t {
	case TabTenants:
		return len(m.tenants)
	case TabDeposits:
		return len(m.deposits)
	case TabCards:
		return len(m.cards)
	case TabDecisions:
		return len(m.decisions)
	case TabActivity:
		return len(m.activity)
	default:
		return 0
	}
}

// View implements tea.Model.
func (m Model) View() string {
	tabBar := view.TabBar(TabLabels(), int(m.activeTab))

	var body string
	switch m.activeTab {
	case TabTenants:
		body = view.Tenants(m.tenants, m.balances, m.cursor[TabTenants])
	case TabDeposits:
		body = view.Deposits(m.deposits, m.tenantNames, m.cursor[TabDeposits])
	case TabCards:
		body = view.Cards(m.cards, m.tenantNames, m.cursor[TabCards])
	case TabDecisions:
		body = view.Decisions(m.decisions, m.tenantNames, m.cursor[TabDecisions])
	case TabActivity:
		body = view.Activity(m.activity, m.cursor[TabActivity], m.activityFeedHeight())
	}

	return lipgloss.JoinVertical(lipgloss.Left,
		tabBar,
		"",
		body,
		"",
		m.footerLine(),
	)
}

// activityFeedHeight reserves a fixed number of lines for the tab bar,
// blank spacer and footer so the activity list fills the rest of the
// terminal instead of over/under-shooting it.
func (m Model) activityFeedHeight() int {
	const reserved = 6
	h := m.height - reserved
	if h < 3 {
		h = 15
	}
	return h
}

func (m Model) footerLine() string {
	status := "● live"
	if !m.connected {
		status = "○ reconnecting…"
	}

	refreshed := "never"
	if !m.lastRefresh.IsZero() {
		refreshed = m.lastRefresh.Format("15:04:05")
	}

	hints := "tab/shift+tab or 1-5: switch tab  •  ↑/↓: scroll  •  r: refresh now  •  q: quit"
	line := fmt.Sprintf("%s   last refresh: %s   %s", status, refreshed, hints)

	if m.err != nil {
		return view.ErrorStyle.Render(fmt.Sprintf("last error: %v", m.err)) + "\n" + view.MutedStyle.Render(line)
	}
	return view.MutedStyle.Render(line)
}

// fetchAll polls ledger-core and the gateway for the current snapshot of
// tenants, deposits, cards and balances. tenant-scoped RPCs are issued
// once per tenant; at demo/hackathon scale (a handful of tenants) this is
// simpler and more auditable than inventing a batched endpoint, and it's
// still bounded by fetchTimeout.
func fetchAll(parent context.Context, c *client.Clients) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, fetchTimeout)
		defer cancel()

		tenants, err := c.ListTenants(ctx)
		if err != nil {
			return refreshMsg{err: err}
		}

		var deposits []*ledgerpb.Deposit
		var cards []*gatewaypb.Card
		var decisions []*ledgerpb.AgentDecision
		balances := make(map[string]*ledgerpb.Balance, len(tenants))

		for _, t := range tenants {
			id := t.GetId()

			if d, err := c.ListDeposits(ctx, id); err == nil {
				deposits = append(deposits, d...)
			}
			if cd, err := c.ListCards(ctx, id); err == nil {
				cards = append(cards, cd...)
			}
			if bal, err := c.GetBalance(ctx, id); err == nil {
				balances[id] = bal
			}
			if dec, err := c.ListAgentDecisions(ctx, id, decisionsPerTenant); err == nil {
				decisions = append(decisions, dec...)
			}
		}

		sortDecisionsNewestFirst(decisions)

		return refreshMsg{
			tenants:   tenants,
			deposits:  deposits,
			cards:     cards,
			balances:  balances,
			decisions: decisions,
		}
	}
}

func tickEvery(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return tickMsg(t) })
}
