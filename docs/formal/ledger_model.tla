-------------------------- MODULE ledger_model --------------------------
(***************************************************************************)
(* Concrete TLC model instance of `ledger.tla`. This module exists only    *)
(* because TLC's .cfg grammar can't assign a function-valued CONSTANT      *)
(* (`ReserveFloor`) as a literal directly -- the standard TLA+/TLC idiom    *)
(* for that is a small "MC" module like this one that EXTENDS the real     *)
(* spec, declares the model-value constants, and defines the function     *)
(* literal as an ordinary operator that the .cfg file then points         *)
(* `ReserveFloor` at via `<-`. Everything that is actually checked lives   *)
(* in ledger.tla; this file only wires up concrete constants so TLC has a  *)
(* finite model to run.                                                    *)
(***************************************************************************)
EXTENDS ledger

CONSTANTS t1, t2, h1, h2

(* Two tenants with DIFFERENT reserve floors -- t1 has a positive floor    *)
(* (so the headroom-gated DEBIT/HOLD branch is actually exercised and can  *)
(* actually reject something), t2's floor is zero (the "no protected       *)
(* capital" case `create_tenant` explicitly allows).                       *)
(*                                                                         *)
(* NOTE: deliberately NOT written as the record literal                   *)
(* `[t1 |-> 2, t2 |-> 0]` -- that syntax's domain is the STRING set        *)
(* {"t1", "t2"}, not the model-value set {t1, t2}, which would make        *)
(* `ReserveFloor \in [Tenants -> Nat]` false. This is an explicit function *)
(* over the model values themselves. *)
ReserveFloorDef == [t \in {t1, t2} |-> IF t = t1 THEN 2 ELSE 0]

===========================================================================
