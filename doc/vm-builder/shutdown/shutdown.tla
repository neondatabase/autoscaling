----------------------------- MODULE vmshutdown -----------------------------

EXTENDS Sequences, Integers, TLC

CONSTANT NULL

(*--algorithm vmshutdown

variables
    start_allowed = TRUE, \* vmstart.allowed
    start_allowed_locked = FALSE, \* vmstart.lock

    \* ACPI & unix signal delivery, modeled through variables that are polled/await'ed
    pg_ctl_stop_signal = FALSE,
    shutdown_signal_received = FALSE,

    \* for temporal invariants
    vmstarter_sh_running = FALSE


fair process init = "init"
begin
    init:
    while ~shutdown_signal_received do
        either
            \* disable respawn loop & run vmshutdown
            shutdown_signal_received := TRUE;
        or
            skip;
        end either;
    end while;
end process;

fair process respawn_vmstart = "respawn_vmstart"
variables
    debug_shutdown_request_observed = TRUE
begin
    init:
    while ~shutdown_signal_received do

        respawn_flock_enter:
            await start_allowed_locked = FALSE;
            start_allowed_locked := TRUE;
        respawn_check_start_allowed:
            if start_allowed then
                vmstarter_sh_running := TRUE;
        respawn_wait_vmstart_exit:
                either
                    await pg_ctl_stop_signal;
                or
                    \* compute_ctl or postgres crashed or exited on its own
                    \* TODO: ensure through temporal property that we can respawn it without a shutdown request
                    skip;
                end either;
                vmstarter_sh_running := FALSE;
            else
        respawn_not_allowed:
                debug_shutdown_request_observed := TRUE;
            end if;
        respawn_flock_exit:
            start_allowed_locked := FALSE;
    end while;

end process;

fair process vmshutdown = "vmshutdown"
begin
    init:
        await shutdown_signal_received;

    vmshutdown_inhibit_new_starts:
        start_allowed := FALSE; \* rm the vmstart.allowed file on disk
    vmshutdown_pg_ctl_stop:
        pg_ctl_stop_signal := TRUE;
    vmshutdown_wait_existing_command:
        await start_allowed_locked = FALSE; \* flock the file blocking exclusive; if an existing command is running, this waits until it's completed
    vmshutdown_done:
        skip;
end process;


end algorithm; *)
\* BEGIN TRANSLATION (chksum(pcal) = "fe7b2e4d" /\ chksum(tla) = "799a3841")
\* Label init of process init at line 24 col 5 changed to init_
\* Label init of process respawn_vmstart at line 39 col 5 changed to init_r
\* Label init of process vmshutdown at line 69 col 9 changed to init_v
VARIABLES start_allowed, start_allowed_locked, pg_ctl_stop_signal,
          shutdown_signal_received, vmstarter_sh_running, pc,
          debug_shutdown_request_observed

vars == << start_allowed, start_allowed_locked, pg_ctl_stop_signal,
           shutdown_signal_received, vmstarter_sh_running, pc,
           debug_shutdown_request_observed >>

ProcSet == {"init"} \cup {"respawn_vmstart"} \cup {"vmshutdown"}

Init == (* Global variables *)
        /\ start_allowed = TRUE
        /\ start_allowed_locked = FALSE
        /\ pg_ctl_stop_signal = FALSE
        /\ shutdown_signal_received = FALSE
        /\ vmstarter_sh_running = FALSE
        (* Process respawn_vmstart *)
        /\ debug_shutdown_request_observed = TRUE
        /\ pc = [self \in ProcSet |-> CASE self = "init" -> "init_"
                                        [] self = "respawn_vmstart" -> "init_r"
                                        [] self = "vmshutdown" -> "init_v"]

init_ == /\ pc["init"] = "init_"
         /\ IF ~shutdown_signal_received
               THEN /\ \/ /\ shutdown_signal_received' = TRUE
                       \/ /\ TRUE
                          /\ UNCHANGED shutdown_signal_received
                    /\ pc' = [pc EXCEPT !["init"] = "init_"]
               ELSE /\ pc' = [pc EXCEPT !["init"] = "Done"]
                    /\ UNCHANGED shutdown_signal_received
         /\ UNCHANGED << start_allowed, start_allowed_locked,
                         pg_ctl_stop_signal, vmstarter_sh_running,
                         debug_shutdown_request_observed >>

init == init_

init_r == /\ pc["respawn_vmstart"] = "init_r"
          /\ IF ~shutdown_signal_received
                THEN /\ pc' = [pc EXCEPT !["respawn_vmstart"] = "respawn_flock_enter"]
                ELSE /\ pc' = [pc EXCEPT !["respawn_vmstart"] = "Done"]
          /\ UNCHANGED << start_allowed, start_allowed_locked,
                          pg_ctl_stop_signal, shutdown_signal_received,
                          vmstarter_sh_running,
                          debug_shutdown_request_observed >>

respawn_flock_enter == /\ pc["respawn_vmstart"] = "respawn_flock_enter"
                       /\ start_allowed_locked = FALSE
                       /\ start_allowed_locked' = TRUE
                       /\ pc' = [pc EXCEPT !["respawn_vmstart"] = "respawn_check_start_allowed"]
                       /\ UNCHANGED << start_allowed, pg_ctl_stop_signal,
                                       shutdown_signal_received,
                                       vmstarter_sh_running,
                                       debug_shutdown_request_observed >>

respawn_check_start_allowed == /\ pc["respawn_vmstart"] = "respawn_check_start_allowed"
                               /\ IF start_allowed
                                     THEN /\ vmstarter_sh_running' = TRUE
                                          /\ pc' = [pc EXCEPT !["respawn_vmstart"] = "respawn_wait_vmstart_exit"]
                                     ELSE /\ pc' = [pc EXCEPT !["respawn_vmstart"] = "respawn_not_allowed"]
                                          /\ UNCHANGED vmstarter_sh_running
                               /\ UNCHANGED << start_allowed,
                                               start_allowed_locked,
                                               pg_ctl_stop_signal,
                                               shutdown_signal_received,
                                               debug_shutdown_request_observed >>

respawn_wait_vmstart_exit == /\ pc["respawn_vmstart"] = "respawn_wait_vmstart_exit"
                             /\ \/ /\ pg_ctl_stop_signal
                                \/ /\ TRUE
                             /\ vmstarter_sh_running' = FALSE
                             /\ pc' = [pc EXCEPT !["respawn_vmstart"] = "respawn_flock_exit"]
                             /\ UNCHANGED << start_allowed,
                                             start_allowed_locked,
                                             pg_ctl_stop_signal,
                                             shutdown_signal_received,
                                             debug_shutdown_request_observed >>

respawn_not_allowed == /\ pc["respawn_vmstart"] = "respawn_not_allowed"
                       /\ debug_shutdown_request_observed' = TRUE
                       /\ pc' = [pc EXCEPT !["respawn_vmstart"] = "respawn_flock_exit"]
                       /\ UNCHANGED << start_allowed, start_allowed_locked,
                                       pg_ctl_stop_signal,
                                       shutdown_signal_received,
                                       vmstarter_sh_running >>

respawn_flock_exit == /\ pc["respawn_vmstart"] = "respawn_flock_exit"
                      /\ start_allowed_locked' = FALSE
                      /\ pc' = [pc EXCEPT !["respawn_vmstart"] = "init_r"]
                      /\ UNCHANGED << start_allowed, pg_ctl_stop_signal,
                                      shutdown_signal_received,
                                      vmstarter_sh_running,
                                      debug_shutdown_request_observed >>

respawn_vmstart == init_r \/ respawn_flock_enter
                      \/ respawn_check_start_allowed
                      \/ respawn_wait_vmstart_exit \/ respawn_not_allowed
                      \/ respawn_flock_exit

init_v == /\ pc["vmshutdown"] = "init_v"
          /\ shutdown_signal_received
          /\ pc' = [pc EXCEPT !["vmshutdown"] = "vmshutdown_inhibit_new_starts"]
          /\ UNCHANGED << start_allowed, start_allowed_locked,
                          pg_ctl_stop_signal, shutdown_signal_received,
                          vmstarter_sh_running,
                          debug_shutdown_request_observed >>

vmshutdown_inhibit_new_starts == /\ pc["vmshutdown"] = "vmshutdown_inhibit_new_starts"
                                 /\ start_allowed' = FALSE
                                 /\ pc' = [pc EXCEPT !["vmshutdown"] = "vmshutdown_pg_ctl_stop"]
                                 /\ UNCHANGED << start_allowed_locked,
                                                 pg_ctl_stop_signal,
                                                 shutdown_signal_received,
                                                 vmstarter_sh_running,
                                                 debug_shutdown_request_observed >>

vmshutdown_pg_ctl_stop == /\ pc["vmshutdown"] = "vmshutdown_pg_ctl_stop"
                          /\ pg_ctl_stop_signal' = TRUE
                          /\ pc' = [pc EXCEPT !["vmshutdown"] = "vmshutdown_wait_existing_command"]
                          /\ UNCHANGED << start_allowed, start_allowed_locked,
                                          shutdown_signal_received,
                                          vmstarter_sh_running,
                                          debug_shutdown_request_observed >>

vmshutdown_wait_existing_command == /\ pc["vmshutdown"] = "vmshutdown_wait_existing_command"
                                    /\ start_allowed_locked = FALSE
                                    /\ pc' = [pc EXCEPT !["vmshutdown"] = "vmshutdown_done"]
                                    /\ UNCHANGED << start_allowed,
                                                    start_allowed_locked,
                                                    pg_ctl_stop_signal,
                                                    shutdown_signal_received,
                                                    vmstarter_sh_running,
                                                    debug_shutdown_request_observed >>

vmshutdown_done == /\ pc["vmshutdown"] = "vmshutdown_done"
                   /\ TRUE
                   /\ pc' = [pc EXCEPT !["vmshutdown"] = "Done"]
                   /\ UNCHANGED << start_allowed, start_allowed_locked,
                                   pg_ctl_stop_signal,
                                   shutdown_signal_received,
                                   vmstarter_sh_running,
                                   debug_shutdown_request_observed >>

vmshutdown == init_v \/ vmshutdown_inhibit_new_starts
                 \/ vmshutdown_pg_ctl_stop
                 \/ vmshutdown_wait_existing_command \/ vmshutdown_done

(* Allow infinite stuttering to prevent deadlock on termination. *)
Terminating == /\ \A self \in ProcSet: pc[self] = "Done"
               /\ UNCHANGED vars

Next == init \/ respawn_vmstart \/ vmshutdown
           \/ Terminating

Spec == /\ Init /\ [][Next]_vars
        /\ WF_vars(init)
        /\ WF_vars(respawn_vmstart)
        /\ WF_vars(vmshutdown)

Termination == <>(\A self \in ProcSet: pc[self] = "Done")

\* END TRANSLATION

\* TEMPORAL PROPERTIES:
\* If we signal ACPI shutdown, vmstart eventually stops running and never restarts
ShutdownSignalWorks == (shutdown_signal_received ~> ([](~vmstarter_sh_running)))
\* Before we signal ACPI shutdown, respawn works
RespawnBeforeShutdownCanRestartWithoutPendingShutdown == TRUE \* TODO: how to express this?

=============================================================================
\* Modification History
\* Last modified Mon Sep 25 09:22:40 CEST 2023 by cs
\* Created Sun Sep 24 12:17:50 CEST 2023 by cs
