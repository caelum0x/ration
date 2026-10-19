// Package demoseed defines the fixed set of demo tenants `ration seed`
// creates. Kept as pure data + a pure constructor function (no I/O) so it
// is unit-testable without a running gateway/ledger-core.
package demoseed

// Tenant is one demo tenant definition handed to LedgerCore's CreateTenant
// RPC by the seed command.
type Tenant struct {
	Name                   string
	Currency               string
	ReserveFloorMinorUnits int64

	// OpeningCapitalMinorUnits is settled operating capital the seed
	// command credits immediately (via RecordDeposit +
	// MarkDepositReceived) so the tenant starts demo-ready: available
	// balance already sits above ReserveFloorMinorUnits, giving the
	// agent's bridge envelope real headroom to grant against on the
	// very first late-deposit scenario a judge runs. Without this, every
	// freshly seeded tenant starts at a balance of exactly 0 — which is
	// always below the reserve floor, so every bridge request would
	// decline no matter how the late deposit itself is sized.
	OpeningCapitalMinorUnits int64
}

// Tenants returns the 3 demo tenants `ration seed` creates: a small,
// medium and large platform, each in a different settlement currency, so
// the reserve-floor math in docs/AGENT-DESIGN.md has realistic variety to
// demo against. Each tenant's opening capital is 3x its reserve floor,
// i.e. 2x the floor in actual headroom — comfortably enough for the
// demo's bridge-funding scenario to produce a real grant, not just a
// decline.
func Tenants() []Tenant {
	return []Tenant{
		{
			Name:                     "Lumen Creator Payouts",
			Currency:                 "USD",
			ReserveFloorMinorUnits:   250_000, // $2,500.00 — small platform
			OpeningCapitalMinorUnits: 750_000, // $7,500.00 settled capital
		},
		{
			Name:                     "Northwind Freight Marketplace",
			Currency:                 "EUR",
			ReserveFloorMinorUnits:   2_500_000, // €25,000.00 — medium platform
			OpeningCapitalMinorUnits: 7_500_000, // €75,000.00 settled capital
		},
		{
			Name:                     "Atlas Gig Exchange",
			Currency:                 "GBP",
			ReserveFloorMinorUnits:   15_000_000, // £150,000.00 — large platform
			OpeningCapitalMinorUnits: 45_000_000, // £450,000.00 settled capital
		},
	}
}
