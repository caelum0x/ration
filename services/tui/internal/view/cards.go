package view

import (
	gatewaypb "github.com/caelum0x/ration/pkg/pb/gateway"
	"github.com/caelum0x/ration/services/tui/internal/format"
)

var cardHeaders = []string{"TENANT", "CARDHOLDER", "STATUS", "LIMIT", "INTERVAL", "CREATED"}
var cardWidths = []int{16, 18, 10, 14, 9, 19}

// Cards renders the cards tab: every issued card across all tenants, with
// status color-coded so a RATIONED card (spend capped below the request
// due to bridge funding, per docs/AGENT-DESIGN.md) is visible at a glance
// next to plain ACTIVE/SUSPENDED/CLOSED cards.
func Cards(cards []*gatewaypb.Card, tenantNames map[string]string, cursor int) string {
	rows := make([][]string, 0, len(cards))
	for _, c := range cards {
		rows = append(rows, []string{
			tenantNames[c.GetTenantId()],
			c.GetCardholderName(),
			format.CardStatusLabel(c.GetStatus()),
			format.Money(c.GetLimitMinorUnits(), c.GetCurrency()),
			c.GetLimitInterval(),
			format.Unix(c.GetCreatedAtUnix()),
		})
	}
	return Table(cardHeaders, rows, cardWidths, cursor, 2, cardStatusStyle)
}
