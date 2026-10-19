package demoseed

import "testing"

func TestTenants_ReturnsThreeDistinctTenants(t *testing.T) {
	tenants := Tenants()

	if len(tenants) != 3 {
		t.Fatalf("want 3 demo tenants, got %d", len(tenants))
	}

	seenNames := make(map[string]bool, len(tenants))
	for _, tenant := range tenants {
		if tenant.Name == "" {
			t.Errorf("tenant has empty name: %+v", tenant)
		}
		if len(tenant.Currency) != 3 {
			t.Errorf("tenant %q currency %q is not a 3-letter ISO 4217 code", tenant.Name, tenant.Currency)
		}
		if tenant.ReserveFloorMinorUnits <= 0 {
			t.Errorf("tenant %q reserve floor must be positive, got %d", tenant.Name, tenant.ReserveFloorMinorUnits)
		}
		if seenNames[tenant.Name] {
			t.Errorf("duplicate tenant name %q", tenant.Name)
		}
		seenNames[tenant.Name] = true
	}
}

func TestTenants_OpeningCapitalExceedsReserveFloor(t *testing.T) {
	// The whole point of OpeningCapitalMinorUnits: a freshly seeded
	// tenant must start with real headroom (available balance above its
	// reserve floor), or the agent's bridge envelope can only ever
	// decline, no matter what late-deposit scenario a judge runs next.
	for _, tenant := range Tenants() {
		if tenant.OpeningCapitalMinorUnits <= tenant.ReserveFloorMinorUnits {
			t.Errorf(
				"tenant %q: opening capital %d must exceed reserve floor %d, or it starts with zero headroom",
				tenant.Name, tenant.OpeningCapitalMinorUnits, tenant.ReserveFloorMinorUnits,
			)
		}
	}
}

func TestTenants_SpanSmallMediumLarge(t *testing.T) {
	tenants := Tenants()

	for i := 1; i < len(tenants); i++ {
		if tenants[i].ReserveFloorMinorUnits <= tenants[i-1].ReserveFloorMinorUnits {
			t.Fatalf(
				"expected strictly increasing reserve floors (small -> medium -> large), got %d then %d",
				tenants[i-1].ReserveFloorMinorUnits, tenants[i].ReserveFloorMinorUnits,
			)
		}
	}
}

func TestTenants_IsDeterministic(t *testing.T) {
	first := Tenants()
	second := Tenants()

	if len(first) != len(second) {
		t.Fatalf("Tenants() returned different lengths across calls: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Errorf("Tenants() is not deterministic at index %d: %+v vs %+v", i, first[i], second[i])
		}
	}
}
