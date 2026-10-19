---------------------------- MODULE ledger ----------------------------
(***************************************************************************)
(* Formal spec of ledger-core's core state machine, as implemented in      *)
(* services/ledger-core/src/ledger.rs (`Ledger::apply_entry`).             *)
(*                                                                         *)
(* What this models:                                                       *)
(*   - A tenant's `available_minor_units` and `held_minor_units` balances. *)
(*   - CREDIT   : available += amt, unconditionally (apply_entry's         *)
(*                `LedgerEntryType::Credit` arm — no floor check, matches  *)
(*                the code: increasing available can never breach a        *)
(*                floor).                                                  *)
(*   - DEBIT    : available -= amt, gated on                               *)
(*                `amt <= available - reserve_floor` ("headroom"). If the  *)
(*                check fails the code returns                            *)
(*                `ApplyOutcome::ReserveFloorViolated` with the balance    *)
(*                UNCHANGED (not an error, not a mutation) — modeled here  *)
(*                as a same-state self-transition, exactly like the Rust   *)
(*                arm does.                                                *)
(*   - HOLD     : available -= amt, held += amt, same headroom gate as     *)
(*                DEBIT. Creates one `pending` hold record, keyed by a     *)
(*                hold id drawn from the finite set `HoldIds` (ledger.rs   *)
(*                itself doesn't expose hold ids to callers — a HOLD's     *)
(*                "identity" there is really its `idempotency_key` /       *)
(*                ledger-entry id; `HoldIds` is this spec's finite stand-  *)
(*                in for "one specific HOLD entry" so each can be tracked  *)
(*                to resolution for the liveness property below).         *)
(*   - RELEASE  : held -= amt, available += amt for a `pending` hold —     *)
(*                matches `LedgerEntryType::Release` exactly (the code     *)
(*                checks `amt <= held`, which is automatically true here   *)
(*                because `amt` is always the exact amount that very HOLD  *)
(*                reserved).                                               *)
(*   - CAPTURE  : held -= amt for a `pending` hold, WITHOUT returning it   *)
(*                to `available` — the formal idealization of "the hold    *)
(*                converts to a DEBIT" from SCALE-EXPANSION.md section 6's *)
(*                liveness requirement. IMPORTANT, read before trusting a  *)
(*                green TLC run: `services/ledger-core` as built does NOT  *)
(*                currently expose a CAPTURE/convert-to-debit entry type   *)
(*                or RPC. The only hold-resolution primitive that exists   *)
(*                today is RELEASE (see                                    *)
(*                `services/gateway/internal/card/service.go` and          *)
(*                `services/gateway/internal/bridge/service.go`, which     *)
(*                place HOLD entries backing card limits / bridge grants   *)
(*                and resolve them only via RELEASE — a real card spend    *)
(*                clearing would need a RELEASE followed by a separate     *)
(*                DEBIT call, not a single atomic conversion). CAPTURE is  *)
(*                included here because SCALE-EXPANSION.md section 6      *)
(*                explicitly specifies the liveness property in terms of  *)
(*                it ("every HOLD eventually RELEASEs or converts to       *)
(*                DEBIT") — this spec models that target state machine so *)
(*                the property is meaningful, but a passing TLC run is a   *)
(*                check of the SPEC's logic, not proof that ledger-core's  *)
(*                Rust code implements CAPTURE, because it doesn't yet.    *)
(*                Precise TODO for whoever adds it: add a                  *)
(*                `LedgerEntryType::Capture` (or equivalent) arm to        *)
(*                `Ledger::apply_entry` in services/ledger-core/src/       *)
(*                ledger.rs that behaves exactly like this module's        *)
(*                `Capture` action, then this spec needs no changes.       *)
(*                                                                         *)
(* What this deliberately does NOT model (out of scope for this spec,      *)
(* already covered elsewhere):                                             *)
(*   - The WAL/SQLite durability mechanics (append, fsync, replay on       *)
(*     crash) — covered by services/ledger-core/src/ledger.rs's own        *)
(*     `replay_wal_into_index` logic, the `recovery.rs` integration tests, *)
(*     and the chaos script in SCALE-EXPANSION.md section 5.              *)
(*   - Idempotency-key deduplication — `apply_entry` short-circuits on a   *)
(*     repeated `idempotency_key` before any of the logic below ever      *)
(*     runs; replaying the same logical entry twice is a no-op by          *)
(*     construction and doesn't interact with the floor invariant.         *)
(*   - Multi-currency / currency-mismatch rejection — orthogonal to the    *)
(*     balance arithmetic this spec checks.                                *)
(*   - The sub-floor bootstrap window: `create_tenant` starts every        *)
(*     tenant at `available = 0`, which can be BELOW a positive            *)
(*     `reserve_floor_minor_units` until the first qualifying CREDIT.      *)
(*     During that window `SafetyReserveFloor` below is not a true         *)
(*     invariant of the real system in the trivial "holds from t=0" sense  *)
(*     -- it is false at the moment of tenant creation whenever            *)
(*     `reserve_floor_minor_units > 0`. The code's actual guarantee is     *)
(*     narrower and is what this spec checks: once available has reached  *)
(*     the floor, no DEBIT or HOLD can ever push it back below the floor   *)
(*     (the headroom check rejects any amount that would do so, as a       *)
(*     no-op, even `amt = 0` whenever headroom is already negative — see   *)
(*     `debit_breaching_reserve_floor_is_rejected_without_mutating_balance`*)
(*     in services/ledger-core/src/ledger.rs's test module, which already  *)
(*     exercises exactly this). `Init` below therefore starts each tenant  *)
(*     EXACTLY at its reserve floor — the tightest / most adversarial      *)
(*     "already funded" starting point — rather than at 0, so the TLC     *)
(*     invariant check is a true statement about the system's steady-     *)
(*     state operation, not a restatement of a known, separately-tested    *)
(*     bootstrap-window fact.                                              *)
(***************************************************************************)

EXTENDS Integers, FiniteSets, TLC

CONSTANTS
    Tenants,        \* finite set of tenant ids
    ReserveFloor,   \* [Tenants -> Nat] -- reserve_floor_minor_units per tenant
    HoldIds,        \* finite set of hold identifiers available to this run
    MaxAmount,       \* bound on any single CREDIT/DEBIT/HOLD amount (model-checking only)
    MaxBalance       \* bound on available/held per tenant (model-checking only, see StateConstraint)

ASSUME ReserveFloorType == ReserveFloor \in [Tenants -> Nat]
ASSUME MaxAmountType == MaxAmount \in Nat
ASSUME MaxBalanceType == MaxBalance \in Nat

VARIABLES
    available,   \* [Tenants -> Int]   -- available_minor_units
    held,        \* [Tenants -> Int]   -- held_minor_units
    holdStatus,  \* [HoldIds -> {"none", "pending", "released", "captured"}]
    holdTenant,  \* [HoldIds -> Tenants] -- meaningful only once holdStatus[h] # "none"
    holdAmount   \* [HoldIds -> Nat]     -- meaningful only once holdStatus[h] # "none"

vars == <<available, held, holdStatus, holdTenant, holdAmount>>

Amounts == 0..MaxAmount
HoldStatuses == {"none", "pending", "released", "captured"}

TypeOK ==
    /\ available \in [Tenants -> Int]
    /\ held \in [Tenants -> Int]
    /\ holdStatus \in [HoldIds -> HoldStatuses]
    /\ holdTenant \in [HoldIds -> Tenants]
    /\ holdAmount \in [HoldIds -> Nat]

(***************************************************************************)
(* Init starts every tenant exactly AT its reserve floor -- see the module *)
(* header for why this (not `available = 0`) is the honest starting point  *)
(* for the invariant this spec checks.                                     *)
(***************************************************************************)
Init ==
    /\ available = [t \in Tenants |-> ReserveFloor[t]]
    /\ held = [t \in Tenants |-> 0]
    /\ holdStatus = [h \in HoldIds |-> "none"]
    /\ holdTenant = [h \in HoldIds |-> CHOOSE t \in Tenants : TRUE]
    /\ holdAmount = [h \in HoldIds |-> 0]

(***************************************************************************)
(* CREDIT: mirrors the `LedgerEntryType::Credit` arm of apply_entry --     *)
(* unconditional, checked-overflow addition to `available`. No floor       *)
(* check: increasing available can never breach `available >= floor`.      *)
(***************************************************************************)
Credit(t, amt) ==
    /\ available' = [available EXCEPT ![t] = @ + amt]
    /\ UNCHANGED <<held, holdStatus, holdTenant, holdAmount>>

(***************************************************************************)
(* DEBIT: mirrors the `LedgerEntryType::Debit` arm. `headroom` is          *)
(* `available[t] - ReserveFloor[t]`, exactly as in                         *)
(* `Ledger::apply_entry`/`Ledger::check_reserve_floor`. If `amt > headroom`*)
(* the real code returns `ApplyOutcome::ReserveFloorViolated` leaving the  *)
(* balance untouched -- modeled here as leaving `available` unchanged      *)
(* rather than refusing the step, since from the ledger's own state-       *)
(* machine point of view a rejected entry is a legitimate (no-op)          *)
(* transition, not a blocked one.                                          *)
(***************************************************************************)
Debit(t, amt) ==
    /\ LET headroom == available[t] - ReserveFloor[t] IN
         IF amt <= headroom
         THEN available' = [available EXCEPT ![t] = @ - amt]
         ELSE available' = available
    /\ UNCHANGED <<held, holdStatus, holdTenant, holdAmount>>

(***************************************************************************)
(* HOLD: mirrors the `LedgerEntryType::Hold` arm -- same headroom gate as  *)
(* DEBIT, but on success moves `amt` from `available` into `held` instead  *)
(* of extinguishing it, and records a new `pending` hold under a fresh     *)
(* (`"none"`) hold id so its eventual resolution can be tracked by the     *)
(* liveness property below.                                                *)
(***************************************************************************)
Hold(t, h, amt) ==
    /\ holdStatus[h] = "none"
    /\ LET headroom == available[t] - ReserveFloor[t] IN
         IF amt <= headroom
         THEN /\ available' = [available EXCEPT ![t] = @ - amt]
              /\ held' = [held EXCEPT ![t] = @ + amt]
              /\ holdStatus' = [holdStatus EXCEPT ![h] = "pending"]
              /\ holdTenant' = [holdTenant EXCEPT ![h] = t]
              /\ holdAmount' = [holdAmount EXCEPT ![h] = amt]
         ELSE UNCHANGED <<available, held, holdStatus, holdTenant, holdAmount>>

(***************************************************************************)
(* RELEASE: mirrors the `LedgerEntryType::Release` arm -- returns a        *)
(* pending hold's exact reserved amount from `held` back to `available`.   *)
(* The real code's `amt > held` guard is automatically satisfied here      *)
(* because `amt` is always precisely the amount that very hold reserved.   *)
(***************************************************************************)
Release(h) ==
    /\ holdStatus[h] = "pending"
    /\ LET t == holdTenant[h]
           amt == holdAmount[h]
       IN /\ held' = [held EXCEPT ![t] = @ - amt]
          /\ available' = [available EXCEPT ![t] = @ + amt]
    /\ holdStatus' = [holdStatus EXCEPT ![h] = "released"]
    /\ UNCHANGED <<holdTenant, holdAmount>>

(***************************************************************************)
(* CAPTURE: a pending hold converting directly into a permanent DEBIT --   *)
(* see the big module-header note: NOT currently implemented by            *)
(* services/ledger-core, modeled so the liveness property from             *)
(* SCALE-EXPANSION.md section 6 is meaningful. Held funds are simply        *)
(* extinguished, never returned to `available`.                            *)
(***************************************************************************)
Capture(h) ==
    /\ holdStatus[h] = "pending"
    /\ LET t == holdTenant[h]
           amt == holdAmount[h]
       IN held' = [held EXCEPT ![t] = @ - amt]
    /\ holdStatus' = [holdStatus EXCEPT ![h] = "captured"]
    /\ UNCHANGED <<available, holdTenant, holdAmount>>

Next ==
    \/ \E t \in Tenants, amt \in Amounts : Credit(t, amt)
    \/ \E t \in Tenants, amt \in Amounts : Debit(t, amt)
    \/ \E t \in Tenants, h \in HoldIds, amt \in Amounts : Hold(t, h, amt)
    \/ \E h \in HoldIds : Release(h)
    \/ \E h \in HoldIds : Capture(h)

(***************************************************************************)
(* Fairness: once a hold is `pending`, its Release/Capture action is       *)
(* required to eventually run if it stays continuously enabled (weak       *)
(* fairness) -- it IS continuously enabled for any pending hold forever    *)
(* (nothing else ever disables it except itself firing), so WF here gives  *)
(* the real liveness guarantee: every pending hold is eventually resolved. *)
(* This fairness assumption models an external caller (today, whichever    *)
(* service placed the HOLD -- card-issuance for limit holds, the bridge    *)
(* service for bridge grants) always eventually issuing the matching       *)
(* RELEASE/capture call. ledger-core itself has no background process that *)
(* auto-resolves stale holds -- that liveness source is purely the caller's*)
(* behavior, which is NOT separately verified anywhere else in this repo   *)
(* today. Precise TODO: if a hold-placing caller can leak a hold forever   *)
(* (crash before releasing, policy bug, etc.), this spec's liveness        *)
(* property is vacuous for that run in reality even though the ledger's    *)
(* own transition relation would still happily accept a RELEASE/CAPTURE if *)
(* one ever arrived.                                                       *)
(***************************************************************************)
Spec == Init /\ [][Next]_vars /\ (\A h \in HoldIds : WF_vars(Release(h) \/ Capture(h)))

(***************************************************************************)
(* SAFETY -- the invariant named in SCALE-EXPANSION.md section 6.          *)
(***************************************************************************)
SafetyReserveFloor == \A t \in Tenants : available[t] >= ReserveFloor[t]

(* Sanity companion invariant: held can never go negative, and a tenant's  *)
(* true total (available + held) is debited/credited consistently -- not  *)
(* required by section 6, but a cheap extra check that the model itself    *)
(* isn't quietly broken (e.g. a RELEASE/CAPTURE firing on an amount larger *)
(* than was ever held). *)
SafetyHeldNonNegative == \A t \in Tenants : held[t] >= 0

(***************************************************************************)
(* LIVENESS -- "every HOLD eventually RELEASEs or converts to DEBIT", from *)
(* SCALE-EXPANSION.md section 6. *)
(***************************************************************************)
HoldsResolve ==
    \A h \in HoldIds :
        (holdStatus[h] = "pending") ~> (holdStatus[h] \in {"released", "captured"})

(***************************************************************************)
(* StateConstraint: bounds available/held for TLC's finite state-space      *)
(* model check only (unbounded repeated CREDIT would make the state space   *)
(* infinite) -- not part of the production system's semantics.             *)
(***************************************************************************)
StateConstraint ==
    \A t \in Tenants : available[t] <= MaxBalance /\ held[t] <= MaxBalance

=============================================================================
