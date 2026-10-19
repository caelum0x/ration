package view

import (
	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
	"github.com/caelum0x/ration/services/tui/internal/format"
)

var depositHeaders = []string{"TENANT", "AMOUNT", "EXPECTED", "RECEIVED", "STATUS", "EXTERNAL REF"}
var depositWidths = []int{16, 14, 19, 19, 10, 20}

// Deposits renders the deposits tab: every tracked deposit across all
// tenants, newest-expected first, with LATE deposits called out in red —
// these are exactly what feeds the agent's bridge-funding trigger.
func Deposits(deposits []*ledgerpb.Deposit, tenantNames map[string]string, cursor int) string {
	rows := make([][]string, 0, len(deposits))
	for _, d := range deposits {
		amount := d.GetExpectedAmount()
		currency := ""
		minor := int64(0)
		if amount != nil {
			currency = amount.GetCurrency()
			minor = amount.GetMinorUnits()
		}
		rows = append(rows, []string{
			tenantNames[d.GetTenantId()],
			format.Money(minor, currency),
			format.Unix(d.GetExpectedAtUnix()),
			format.Unix(d.GetReceivedAtUnix()),
			format.DepositStatusLabel(d.GetStatus()),
			d.GetExternalRef(),
		})
	}
	return Table(depositHeaders, rows, depositWidths, cursor, 4, depositStatusStyle)
}
