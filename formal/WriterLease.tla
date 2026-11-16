--------------------------- MODULE WriterLease ---------------------------
(***************************************************************************)
(* The writer lease of cmd/cellad (role.go). The holder renews it every     *)
(* Poll seconds with a request that reaches the store after a delay; the    *)
(* store sets the expiry to its own receipt time plus TTL, and a standby    *)
(* is granted the lease once the expiry has passed. The holder writes only   *)
(* until a deadline it measures itself, and the process stops writing        *)
(* within StopBy seconds of it. The deadline is measured from the moment the *)
(* last granted renewal was SENT (Margin seconds short of TTL) unless        *)
(* FromResponse is TRUE, which measures it from the response, as the code   *)
(* did before. Time is in whole seconds. TTL, Poll, Margin and StopBy are    *)
(* LeaseTTL, leasePoll, writerMargin and writerStopBy, rendered from the Go *)
(* constants into WriterLeaseConstants.tla by TestWriterLeaseConstants.      *)
(***************************************************************************)
EXTENDS Integers, FiniteSets, WriterLeaseConstants

CONSTANTS Replicas, Horizon, MaxDelay, FromResponse

None == 0
Never == -1
NoReq == [sent |-> Never]
NoResp == [sent |-> Never, at |-> Never, grant |-> FALSE]

VARIABLES
    clock,      \* seconds since the start
    expiry,     \* the store's expiry of the lease, from the receipt of a grant
    owner,      \* the replica the store names as holder, or None
    req,        \* replica -> its request on the way to the store, or None
    resp,       \* replica -> the store's answer on the way back, or None
    lastSent,   \* replica -> the clock of its last request
    deadline,   \* replica -> the clock after which it must not write
    writing     \* replica -> TRUE while it writes

vars == <<clock, expiry, owner, req, resp, lastSent, deadline, writing>>

Init == /\ clock = 0
        /\ expiry = Never
        /\ owner = None
        /\ req = [r \in Replicas |-> NoReq]
        /\ resp = [r \in Replicas |-> NoResp]
        /\ lastSent = [r \in Replicas |-> -Poll]
        /\ deadline = [r \in Replicas |-> Never]
        /\ writing = [r \in Replicas |-> FALSE]

\* A replica asks to take or renew the lease, at most once per Poll and with
\* no request or answer of its own outstanding.
Send(r) ==
    /\ req[r] = NoReq /\ resp[r] = NoResp
    /\ clock - lastSent[r] >= Poll
    /\ req' = [req EXCEPT ![r] = [sent |-> clock]]
    /\ lastSent' = [lastSent EXCEPT ![r] = clock]
    /\ UNCHANGED <<clock, expiry, owner, resp, deadline, writing>>

\* The store receives a request: a lapsed lease, or the holder's own, is
\* granted and expires TTL seconds after this receipt.
Receive(r) ==
    /\ req[r] /= NoReq
    /\ LET grant == expiry < clock \/ owner = r IN
       /\ owner' = IF grant THEN r ELSE owner
       /\ expiry' = IF grant THEN clock + TTL ELSE expiry
       /\ resp' = [resp EXCEPT ![r] = [sent |-> req[r].sent, at |-> clock, grant |-> grant]]
    /\ req' = [req EXCEPT ![r] = NoReq]
    /\ UNCHANGED <<clock, lastSent, deadline, writing>>

\* The answer reaches the replica. A granted lease is written under until the
\* deadline; an answer that comes after its deadline starts no writing.
Deliver(r) ==
    /\ resp[r] /= NoResp
    /\ LET d == IF FromResponse THEN clock + TTL ELSE resp[r].sent + TTL - Margin IN
       /\ deadline' = [deadline EXCEPT ![r] = IF resp[r].grant THEN d ELSE Never]
       /\ writing' = [writing EXCEPT ![r] = resp[r].grant /\ d > clock]
    /\ resp' = [resp EXCEPT ![r] = NoResp]
    /\ UNCHANGED <<clock, expiry, owner, req, lastSent>>

\* A request or an answer is lost.
Lose(r) ==
    /\ \/ /\ req[r] /= NoReq /\ req' = [req EXCEPT ![r] = NoReq] /\ UNCHANGED resp
       \/ /\ resp[r] /= NoResp /\ resp' = [resp EXCEPT ![r] = NoResp] /\ UNCHANGED req
    /\ UNCHANGED <<clock, expiry, owner, lastSent, deadline, writing>>

\* The holder stops writing at its deadline.
Stop(r) ==
    /\ writing[r] /\ clock >= deadline[r]
    /\ writing' = [writing EXCEPT ![r] = FALSE]
    /\ UNCHANGED <<clock, expiry, owner, req, resp, lastSent, deadline>>

\* A second passes. A request or an answer takes at most MaxDelay seconds to
\* arrive, and a writer stops within StopBy seconds of its deadline.
Tick ==
    /\ clock < Horizon
    /\ \A r \in Replicas : req[r] /= NoReq => clock - req[r].sent < MaxDelay
    /\ \A r \in Replicas : resp[r] /= NoResp => clock - resp[r].at < MaxDelay
    /\ \A r \in Replicas : writing[r] => clock + 1 <= deadline[r] + StopBy
    /\ clock' = clock + 1
    /\ UNCHANGED <<expiry, owner, req, resp, lastSent, deadline, writing>>

End == clock = Horizon /\ UNCHANGED vars

Next == Tick \/ End \/ \E r \in Replicas : Send(r) \/ Receive(r) \/ Deliver(r) \/ Lose(r) \/ Stop(r)

Spec == Init /\ [][Next]_vars

\* Two writers never write at once.
Exclusive == Cardinality({r \in Replicas : writing[r]}) <= 1

TypeOK == /\ clock \in 0..Horizon
          /\ owner \in Replicas \cup {None}
=============================================================================
