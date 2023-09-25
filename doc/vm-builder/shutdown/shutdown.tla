----------------------------- MODULE vmshutdown -----------------------------

EXTENDS Sequences, Integers, TLC

CONSTANT NULL

(*--algorithm vmshutdown

variables
    start_allowed = TRUE, \* vmstart.allowed
    start_allowed_locked = FALSE, \* vmstart.lock

    \* ACPI & unix signal delivery, modeled through variables that are polled/await'ed
    shutdown_signal_received = FALSE,
    postgres_running = NULL,
    postgres_spawn_pending = NULL,
    postgres_shutdown_request_pending = NULL,
    postgres_next_pids = <<1,2>>, \* bound number of crashes
    postgres_exited_pids = {},

    machine_running = TRUE,
    vmshutdown_exited = FALSE,

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
    wait_for_vmshutdown:
        await vmshutdown_exited;
    poweroff_to_kernel:
        machine_running := FALSE;
end process;

fair process respawn_vmstart = "respawn_vmstart"
variables
    debug_shutdown_request_observed = TRUE,
    respawn_current_postgres_pid = NULL
begin
    init:
    while ~shutdown_signal_received do

        respawn_flock_enter:
            await start_allowed_locked = FALSE;
            start_allowed_locked := TRUE;
        respawn_check_start_allowed:
            if start_allowed then
        respawn_launch_vmstarter_sh:
                vmstarter_sh_running := TRUE;
        respawn_vmstarter_launch_postgres:
                postgres_spawn_pending := Head(postgres_next_pids);
                respawn_current_postgres_pid := postgres_spawn_pending;
                postgres_next_pids := Tail(postgres_next_pids);
        respawn_vmstarter_wait_postgres:
                await respawn_current_postgres_pid \in postgres_exited_pids;
        respawn_vmstarter_sh_exits:
                vmstarter_sh_running := FALSE;
            else
        respawn_not_allowed:
                debug_shutdown_request_observed := TRUE;
            end if;
        respawn_flock_exit:
            start_allowed_locked := FALSE;
    end while;

end process;

fair process postgres = "postgres"
begin
    init:
    while machine_running do
        postgres_wait_to_be_launched:
            await ~machine_running \/ postgres_spawn_pending /= NULL;
            if ~machine_running then
                goto halt;
            else
                postgres_running := postgres_spawn_pending;
                postgres_spawn_pending := NULL;
            end if;

        postgres_await_shutdown_or_crash:

            \* bound number of crashes to pids left, otherwise we have infinite state space "until" shutdown signal gets delivered
            if Len(postgres_next_pids) > 0 then
                either
                    await postgres_shutdown_request_pending = postgres_running;
                or
                    \* crash / exit on its own
                    skip;
                end either;
            else
                await postgres_shutdown_request_pending = postgres_running;
            end if;
            postgres_exited_pids  := postgres_exited_pids \union {postgres_running};
            postgres_running := NULL;
    end while;
    halt:
       skip;
end process;

fair process vmshutdown = "vmshutdown"
begin
    init:
        await shutdown_signal_received;

    vmshutdown_inhibit_new_starts:
        start_allowed := FALSE; \* rm the vmstart.allowed file on disk
    vmshutdown_pg_ctl_stop:
        \* the `if` models signal loss
        if postgres_running /= NULL then
            postgres_shutdown_request_pending := postgres_running;
        end if;
    vmshutdown_wait_for_running_command:
        await start_allowed_locked = FALSE; \* flock the file blocking exclusive; if an existing command is running, this waits until it's completed
    vmshutdown_done:
        vmshutdown_exited := TRUE;
        skip;
end process;


end algorithm; *)
\* BEGIN TRANSLATION (chksum(pcal) = "48a3e1a5" /\ chksum(tla) = "2cd9490d")
\* Label init of process init at line 31 col 5 changed to init_
\* Label init of process respawn_vmstart at line 51 col 5 changed to init_r
\* Label init of process postgres at line 81 col 5 changed to init_p
\* Label init of process vmshutdown at line 114 col 9 changed to init_v
VARIABLES start_allowed, start_allowed_locked, shutdown_signal_received,
          postgres_running, postgres_spawn_pending,
          postgres_shutdown_request_pending, postgres_next_pids,
          postgres_exited_pids, machine_running, vmshutdown_exited,
          vmstarter_sh_running, pc, debug_shutdown_request_observed,
          respawn_current_postgres_pid

vars == << start_allowed, start_allowed_locked, shutdown_signal_received,
           postgres_running, postgres_spawn_pending,
           postgres_shutdown_request_pending, postgres_next_pids,
           postgres_exited_pids, machine_running, vmshutdown_exited,
           vmstarter_sh_running, pc, debug_shutdown_request_observed,
           respawn_current_postgres_pid >>

ProcSet == {"init"} \cup {"respawn_vmstart"} \cup {"postgres"} \cup {"vmshutdown"}

Init == (* Global variables *)
        /\ start_allowed = TRUE
        /\ start_allowed_locked = FALSE
        /\ shutdown_signal_received = FALSE
        /\ postgres_running = NULL
        /\ postgres_spawn_pending = NULL
        /\ postgres_shutdown_request_pending = NULL
        /\ postgres_next_pids = <<1,2>>
        /\ postgres_exited_pids = {}
        /\ machine_running = TRUE
        /\ vmshutdown_exited = FALSE
        /\ vmstarter_sh_running = FALSE
        (* Process respawn_vmstart *)
        /\ debug_shutdown_request_observed = TRUE
        /\ respawn_current_postgres_pid = NULL
        /\ pc = [self \in ProcSet |-> CASE self = "init" -> "init_"
                                        [] self = "respawn_vmstart" -> "init_r"
                                        [] self = "postgres" -> "init_p"
                                        [] self = "vmshutdown" -> "init_v"]

init_ == /\ pc["init"] = "init_"
         /\ IF ~shutdown_signal_received
               THEN /\ \/ /\ shutdown_signal_received' = TRUE
                       \/ /\ TRUE
                          /\ UNCHANGED shutdown_signal_received
                    /\ pc' = [pc EXCEPT !["init"] = "init_"]
               ELSE /\ pc' = [pc EXCEPT !["init"] = "wait_for_vmshutdown"]
                    /\ UNCHANGED shutdown_signal_received
         /\ UNCHANGED << start_allowed, start_allowed_locked, postgres_running,
                         postgres_spawn_pending,
                         postgres_shutdown_request_pending, postgres_next_pids,
                         postgres_exited_pids, machine_running,
                         vmshutdown_exited, vmstarter_sh_running,
                         debug_shutdown_request_observed,
                         respawn_current_postgres_pid >>

wait_for_vmshutdown == /\ pc["init"] = "wait_for_vmshutdown"
                       /\ vmshutdown_exited
                       /\ pc' = [pc EXCEPT !["init"] = "poweroff_to_kernel"]
                       /\ UNCHANGED << start_allowed, start_allowed_locked,
                                       shutdown_signal_received,
                                       postgres_running,
                                       postgres_spawn_pending,
                                       postgres_shutdown_request_pending,
                                       postgres_next_pids,
                                       postgres_exited_pids, machine_running,
                                       vmshutdown_exited, vmstarter_sh_running,
                                       debug_shutdown_request_observed,
                                       respawn_current_postgres_pid >>

poweroff_to_kernel == /\ pc["init"] = "poweroff_to_kernel"
                      /\ machine_running' = FALSE
                      /\ pc' = [pc EXCEPT !["init"] = "Done"]
                      /\ UNCHANGED << start_allowed, start_allowed_locked,
                                      shutdown_signal_received,
                                      postgres_running, postgres_spawn_pending,
                                      postgres_shutdown_request_pending,
                                      postgres_next_pids, postgres_exited_pids,
                                      vmshutdown_exited, vmstarter_sh_running,
                                      debug_shutdown_request_observed,
                                      respawn_current_postgres_pid >>

init == init_ \/ wait_for_vmshutdown \/ poweroff_to_kernel

init_r == /\ pc["respawn_vmstart"] = "init_r"
          /\ IF ~shutdown_signal_received
                THEN /\ pc' = [pc EXCEPT !["respawn_vmstart"] = "respawn_flock_enter"]
                ELSE /\ pc' = [pc EXCEPT !["respawn_vmstart"] = "Done"]
          /\ UNCHANGED << start_allowed, start_allowed_locked,
                          shutdown_signal_received, postgres_running,
                          postgres_spawn_pending,
                          postgres_shutdown_request_pending,
                          postgres_next_pids, postgres_exited_pids,
                          machine_running, vmshutdown_exited,
                          vmstarter_sh_running,
                          debug_shutdown_request_observed,
                          respawn_current_postgres_pid >>

respawn_flock_enter == /\ pc["respawn_vmstart"] = "respawn_flock_enter"
                       /\ start_allowed_locked = FALSE
                       /\ start_allowed_locked' = TRUE
                       /\ pc' = [pc EXCEPT !["respawn_vmstart"] = "respawn_check_start_allowed"]
                       /\ UNCHANGED << start_allowed, shutdown_signal_received,
                                       postgres_running,
                                       postgres_spawn_pending,
                                       postgres_shutdown_request_pending,
                                       postgres_next_pids,
                                       postgres_exited_pids, machine_running,
                                       vmshutdown_exited, vmstarter_sh_running,
                                       debug_shutdown_request_observed,
                                       respawn_current_postgres_pid >>

respawn_check_start_allowed == /\ pc["respawn_vmstart"] = "respawn_check_start_allowed"
                               /\ IF start_allowed
                                     THEN /\ pc' = [pc EXCEPT !["respawn_vmstart"] = "respawn_launch_vmstarter_sh"]
                                     ELSE /\ pc' = [pc EXCEPT !["respawn_vmstart"] = "respawn_not_allowed"]
                               /\ UNCHANGED << start_allowed,
                                               start_allowed_locked,
                                               shutdown_signal_received,
                                               postgres_running,
                                               postgres_spawn_pending,
                                               postgres_shutdown_request_pending,
                                               postgres_next_pids,
                                               postgres_exited_pids,
                                               machine_running,
                                               vmshutdown_exited,
                                               vmstarter_sh_running,
                                               debug_shutdown_request_observed,
                                               respawn_current_postgres_pid >>

respawn_launch_vmstarter_sh == /\ pc["respawn_vmstart"] = "respawn_launch_vmstarter_sh"
                               /\ vmstarter_sh_running' = TRUE
                               /\ pc' = [pc EXCEPT !["respawn_vmstart"] = "respawn_vmstarter_launch_postgres"]
                               /\ UNCHANGED << start_allowed,
                                               start_allowed_locked,
                                               shutdown_signal_received,
                                               postgres_running,
                                               postgres_spawn_pending,
                                               postgres_shutdown_request_pending,
                                               postgres_next_pids,
                                               postgres_exited_pids,
                                               machine_running,
                                               vmshutdown_exited,
                                               debug_shutdown_request_observed,
                                               respawn_current_postgres_pid >>

respawn_vmstarter_launch_postgres == /\ pc["respawn_vmstart"] = "respawn_vmstarter_launch_postgres"
                                     /\ postgres_spawn_pending' = Head(postgres_next_pids)
                                     /\ respawn_current_postgres_pid' = postgres_spawn_pending'
                                     /\ postgres_next_pids' = Tail(postgres_next_pids)
                                     /\ pc' = [pc EXCEPT !["respawn_vmstart"] = "respawn_vmstarter_wait_postgres"]
                                     /\ UNCHANGED << start_allowed,
                                                     start_allowed_locked,
                                                     shutdown_signal_received,
                                                     postgres_running,
                                                     postgres_shutdown_request_pending,
                                                     postgres_exited_pids,
                                                     machine_running,
                                                     vmshutdown_exited,
                                                     vmstarter_sh_running,
                                                     debug_shutdown_request_observed >>

respawn_vmstarter_wait_postgres == /\ pc["respawn_vmstart"] = "respawn_vmstarter_wait_postgres"
                                   /\ respawn_current_postgres_pid \in postgres_exited_pids
                                   /\ pc' = [pc EXCEPT !["respawn_vmstart"] = "respawn_vmstarter_sh_exits"]
                                   /\ UNCHANGED << start_allowed,
                                                   start_allowed_locked,
                                                   shutdown_signal_received,
                                                   postgres_running,
                                                   postgres_spawn_pending,
                                                   postgres_shutdown_request_pending,
                                                   postgres_next_pids,
                                                   postgres_exited_pids,
                                                   machine_running,
                                                   vmshutdown_exited,
                                                   vmstarter_sh_running,
                                                   debug_shutdown_request_observed,
                                                   respawn_current_postgres_pid >>

respawn_vmstarter_sh_exits == /\ pc["respawn_vmstart"] = "respawn_vmstarter_sh_exits"
                              /\ vmstarter_sh_running' = FALSE
                              /\ pc' = [pc EXCEPT !["respawn_vmstart"] = "respawn_flock_exit"]
                              /\ UNCHANGED << start_allowed,
                                              start_allowed_locked,
                                              shutdown_signal_received,
                                              postgres_running,
                                              postgres_spawn_pending,
                                              postgres_shutdown_request_pending,
                                              postgres_next_pids,
                                              postgres_exited_pids,
                                              machine_running,
                                              vmshutdown_exited,
                                              debug_shutdown_request_observed,
                                              respawn_current_postgres_pid >>

respawn_not_allowed == /\ pc["respawn_vmstart"] = "respawn_not_allowed"
                       /\ debug_shutdown_request_observed' = TRUE
                       /\ pc' = [pc EXCEPT !["respawn_vmstart"] = "respawn_flock_exit"]
                       /\ UNCHANGED << start_allowed, start_allowed_locked,
                                       shutdown_signal_received,
                                       postgres_running,
                                       postgres_spawn_pending,
                                       postgres_shutdown_request_pending,
                                       postgres_next_pids,
                                       postgres_exited_pids, machine_running,
                                       vmshutdown_exited, vmstarter_sh_running,
                                       respawn_current_postgres_pid >>

respawn_flock_exit == /\ pc["respawn_vmstart"] = "respawn_flock_exit"
                      /\ start_allowed_locked' = FALSE
                      /\ pc' = [pc EXCEPT !["respawn_vmstart"] = "init_r"]
                      /\ UNCHANGED << start_allowed, shutdown_signal_received,
                                      postgres_running, postgres_spawn_pending,
                                      postgres_shutdown_request_pending,
                                      postgres_next_pids, postgres_exited_pids,
                                      machine_running, vmshutdown_exited,
                                      vmstarter_sh_running,
                                      debug_shutdown_request_observed,
                                      respawn_current_postgres_pid >>

respawn_vmstart == init_r \/ respawn_flock_enter
                      \/ respawn_check_start_allowed
                      \/ respawn_launch_vmstarter_sh
                      \/ respawn_vmstarter_launch_postgres
                      \/ respawn_vmstarter_wait_postgres
                      \/ respawn_vmstarter_sh_exits \/ respawn_not_allowed
                      \/ respawn_flock_exit

init_p == /\ pc["postgres"] = "init_p"
          /\ IF machine_running
                THEN /\ pc' = [pc EXCEPT !["postgres"] = "postgres_wait_to_be_launched"]
                ELSE /\ pc' = [pc EXCEPT !["postgres"] = "halt"]
          /\ UNCHANGED << start_allowed, start_allowed_locked,
                          shutdown_signal_received, postgres_running,
                          postgres_spawn_pending,
                          postgres_shutdown_request_pending,
                          postgres_next_pids, postgres_exited_pids,
                          machine_running, vmshutdown_exited,
                          vmstarter_sh_running,
                          debug_shutdown_request_observed,
                          respawn_current_postgres_pid >>

postgres_wait_to_be_launched == /\ pc["postgres"] = "postgres_wait_to_be_launched"
                                /\ ~machine_running \/ postgres_spawn_pending /= NULL
                                /\ IF ~machine_running
                                      THEN /\ pc' = [pc EXCEPT !["postgres"] = "halt"]
                                           /\ UNCHANGED << postgres_running,
                                                           postgres_spawn_pending >>
                                      ELSE /\ postgres_running' = postgres_spawn_pending
                                           /\ postgres_spawn_pending' = NULL
                                           /\ pc' = [pc EXCEPT !["postgres"] = "postgres_await_shutdown_or_crash"]
                                /\ UNCHANGED << start_allowed,
                                                start_allowed_locked,
                                                shutdown_signal_received,
                                                postgres_shutdown_request_pending,
                                                postgres_next_pids,
                                                postgres_exited_pids,
                                                machine_running,
                                                vmshutdown_exited,
                                                vmstarter_sh_running,
                                                debug_shutdown_request_observed,
                                                respawn_current_postgres_pid >>

postgres_await_shutdown_or_crash == /\ pc["postgres"] = "postgres_await_shutdown_or_crash"
                                    /\ IF Len(postgres_next_pids) > 0
                                          THEN /\ \/ /\ postgres_shutdown_request_pending = postgres_running
                                                  \/ /\ TRUE
                                          ELSE /\ postgres_shutdown_request_pending = postgres_running
                                    /\ postgres_exited_pids' = (postgres_exited_pids \union {postgres_running})
                                    /\ postgres_running' = NULL
                                    /\ pc' = [pc EXCEPT !["postgres"] = "init_p"]
                                    /\ UNCHANGED << start_allowed,
                                                    start_allowed_locked,
                                                    shutdown_signal_received,
                                                    postgres_spawn_pending,
                                                    postgres_shutdown_request_pending,
                                                    postgres_next_pids,
                                                    machine_running,
                                                    vmshutdown_exited,
                                                    vmstarter_sh_running,
                                                    debug_shutdown_request_observed,
                                                    respawn_current_postgres_pid >>

halt == /\ pc["postgres"] = "halt"
        /\ TRUE
        /\ pc' = [pc EXCEPT !["postgres"] = "Done"]
        /\ UNCHANGED << start_allowed, start_allowed_locked,
                        shutdown_signal_received, postgres_running,
                        postgres_spawn_pending,
                        postgres_shutdown_request_pending, postgres_next_pids,
                        postgres_exited_pids, machine_running,
                        vmshutdown_exited, vmstarter_sh_running,
                        debug_shutdown_request_observed,
                        respawn_current_postgres_pid >>

postgres == init_p \/ postgres_wait_to_be_launched
               \/ postgres_await_shutdown_or_crash \/ halt

init_v == /\ pc["vmshutdown"] = "init_v"
          /\ shutdown_signal_received
          /\ pc' = [pc EXCEPT !["vmshutdown"] = "vmshutdown_inhibit_new_starts"]
          /\ UNCHANGED << start_allowed, start_allowed_locked,
                          shutdown_signal_received, postgres_running,
                          postgres_spawn_pending,
                          postgres_shutdown_request_pending,
                          postgres_next_pids, postgres_exited_pids,
                          machine_running, vmshutdown_exited,
                          vmstarter_sh_running,
                          debug_shutdown_request_observed,
                          respawn_current_postgres_pid >>

vmshutdown_inhibit_new_starts == /\ pc["vmshutdown"] = "vmshutdown_inhibit_new_starts"
                                 /\ start_allowed' = FALSE
                                 /\ pc' = [pc EXCEPT !["vmshutdown"] = "vmshutdown_pg_ctl_stop"]
                                 /\ UNCHANGED << start_allowed_locked,
                                                 shutdown_signal_received,
                                                 postgres_running,
                                                 postgres_spawn_pending,
                                                 postgres_shutdown_request_pending,
                                                 postgres_next_pids,
                                                 postgres_exited_pids,
                                                 machine_running,
                                                 vmshutdown_exited,
                                                 vmstarter_sh_running,
                                                 debug_shutdown_request_observed,
                                                 respawn_current_postgres_pid >>

vmshutdown_pg_ctl_stop == /\ pc["vmshutdown"] = "vmshutdown_pg_ctl_stop"
                          /\ IF postgres_running /= NULL
                                THEN /\ postgres_shutdown_request_pending' = postgres_running
                                ELSE /\ TRUE
                                     /\ UNCHANGED postgres_shutdown_request_pending
                          /\ pc' = [pc EXCEPT !["vmshutdown"] = "vmshutdown_wait_for_running_command"]
                          /\ UNCHANGED << start_allowed, start_allowed_locked,
                                          shutdown_signal_received,
                                          postgres_running,
                                          postgres_spawn_pending,
                                          postgres_next_pids,
                                          postgres_exited_pids,
                                          machine_running, vmshutdown_exited,
                                          vmstarter_sh_running,
                                          debug_shutdown_request_observed,
                                          respawn_current_postgres_pid >>

vmshutdown_wait_for_running_command == /\ pc["vmshutdown"] = "vmshutdown_wait_for_running_command"
                                       /\ start_allowed_locked = FALSE
                                       /\ pc' = [pc EXCEPT !["vmshutdown"] = "vmshutdown_done"]
                                       /\ UNCHANGED << start_allowed,
                                                       start_allowed_locked,
                                                       shutdown_signal_received,
                                                       postgres_running,
                                                       postgres_spawn_pending,
                                                       postgres_shutdown_request_pending,
                                                       postgres_next_pids,
                                                       postgres_exited_pids,
                                                       machine_running,
                                                       vmshutdown_exited,
                                                       vmstarter_sh_running,
                                                       debug_shutdown_request_observed,
                                                       respawn_current_postgres_pid >>

vmshutdown_done == /\ pc["vmshutdown"] = "vmshutdown_done"
                   /\ vmshutdown_exited' = TRUE
                   /\ TRUE
                   /\ pc' = [pc EXCEPT !["vmshutdown"] = "Done"]
                   /\ UNCHANGED << start_allowed, start_allowed_locked,
                                   shutdown_signal_received, postgres_running,
                                   postgres_spawn_pending,
                                   postgres_shutdown_request_pending,
                                   postgres_next_pids, postgres_exited_pids,
                                   machine_running, vmstarter_sh_running,
                                   debug_shutdown_request_observed,
                                   respawn_current_postgres_pid >>

vmshutdown == init_v \/ vmshutdown_inhibit_new_starts
                 \/ vmshutdown_pg_ctl_stop
                 \/ vmshutdown_wait_for_running_command \/ vmshutdown_done

(* Allow infinite stuttering to prevent deadlock on termination. *)
Terminating == /\ \A self \in ProcSet: pc[self] = "Done"
               /\ UNCHANGED vars

Next == init \/ respawn_vmstart \/ postgres \/ vmshutdown
           \/ Terminating

Spec == /\ Init /\ [][Next]_vars
        /\ WF_vars(init)
        /\ WF_vars(respawn_vmstart)
        /\ WF_vars(postgres)
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
\* Last modified Mon Sep 25 10:28:09 CEST 2023 by cs
\* Created Sun Sep 24 12:17:50 CEST 2023 by cs
