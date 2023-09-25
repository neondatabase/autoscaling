----------------------------- MODULE vmshutdown -----------------------------

EXTENDS Sequences, Integers, TLC

CONSTANT NULL

(*--algorithm vmshutdown

variables
    start_allowed = TRUE, \* vmstart.allowed
    start_allowed_locked = FALSE, \* vmstart.lock

    \* ACPI & unix signal delivery, modeled through variables that are polled/await'ed
    shutdown_signal_received = FALSE,
    vmstarter_sh_running = NULL,
    vmstarter_sh_spawn_pending = NULL,
    vmstarter_sh_sigterm_pending = NULL,
    vmstarter_sh_next_pids = <<1,2,3>>, \* bound number of crashes
    vmstarter_sh_exited_pids = {},

    machine_running = TRUE,
    vmshutdown_exited = FALSE,



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
    respawn_current_pid = NULL
begin
    init:
    while ~shutdown_signal_received do

        respawn_flock_enter:
            await start_allowed_locked = FALSE;
            start_allowed_locked := TRUE;
        respawn_check_start_allowed:
            if start_allowed then
        respawn_vmstarter_spawn:
                vmstarter_sh_spawn_pending := Head(vmstarter_sh_next_pids);
                respawn_current_pid := vmstarter_sh_spawn_pending;
                vmstarter_sh_next_pids := Tail(vmstarter_sh_next_pids);
        respawn_vmstarter_wait:
                await ~machine_running \/ respawn_current_pid \in vmstarter_sh_exited_pids;
                if ~machine_running then
                    goto halt;
                end if;
            else
        respawn_not_allowed:
                skip;
            end if;
        respawn_flock_exit:
            start_allowed_locked := FALSE;
    end while;
    halt:
        skip;
end process;

fair process vmstarter_sh = "vmstarter_sh"
begin
    init:
    while machine_running do
        \* model process-launching in PlusCal
        vmstarter_sh_wait_to_be_launched:
            await ~machine_running \/ vmstarter_sh_spawn_pending /= NULL;
            if ~machine_running then
                goto halt;
            else
                vmstarter_sh_running := vmstarter_sh_spawn_pending;
                vmstarter_sh_spawn_pending := NULL;
            end if;

        vmstarter_await_shutdown_or_crash:

            \* bound number of crashes to pids left, otherwise we have infinite state space "until" shutdown signal gets delivered
            if Len(vmstarter_sh_next_pids) > 0 then
                either
                    await vmstarter_sh_sigterm_pending = vmstarter_sh_running;
                or
                    \* crash / exit on its own
                    skip;
                end either;
            else
                await vmstarter_sh_sigterm_pending = vmstarter_sh_running;
            end if;
            vmstarter_sh_exited_pids  := vmstarter_sh_exited_pids \union {vmstarter_sh_running};
            vmstarter_sh_running := NULL;
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
    vmshutdown_check_running_command:
        if start_allowed_locked = TRUE then \* implement through trylock
    vmshutdown_try_signal:
            \* the `if` models signal loss
            if vmstarter_sh_running /= NULL then
                vmstarter_sh_sigterm_pending := vmstarter_sh_running;
            end if;
            goto vmshutdown_check_running_command;
        end if;
    vmshutdown_done:
        vmshutdown_exited := TRUE;
end process;


end algorithm; *)
\* BEGIN TRANSLATION (chksum(pcal) = "8ee88c63" /\ chksum(tla) = "f9b1c493")
\* Label init of process init at line 29 col 5 changed to init_
\* Label init of process respawn_vmstart at line 48 col 5 changed to init_r
\* Label halt of process respawn_vmstart at line 72 col 9 changed to halt_
\* Label init of process vmstarter_sh at line 78 col 5 changed to init_v
\* Label init of process vmshutdown at line 112 col 9 changed to init_vm
VARIABLES start_allowed, start_allowed_locked, shutdown_signal_received,
          vmstarter_sh_running, vmstarter_sh_spawn_pending,
          vmstarter_sh_sigterm_pending, vmstarter_sh_next_pids,
          vmstarter_sh_exited_pids, machine_running, vmshutdown_exited, pc,
          respawn_current_pid

vars == << start_allowed, start_allowed_locked, shutdown_signal_received,
           vmstarter_sh_running, vmstarter_sh_spawn_pending,
           vmstarter_sh_sigterm_pending, vmstarter_sh_next_pids,
           vmstarter_sh_exited_pids, machine_running, vmshutdown_exited, pc,
           respawn_current_pid >>

ProcSet == {"init"} \cup {"respawn_vmstart"} \cup {"vmstarter_sh"} \cup {"vmshutdown"}

Init == (* Global variables *)
        /\ start_allowed = TRUE
        /\ start_allowed_locked = FALSE
        /\ shutdown_signal_received = FALSE
        /\ vmstarter_sh_running = NULL
        /\ vmstarter_sh_spawn_pending = NULL
        /\ vmstarter_sh_sigterm_pending = NULL
        /\ vmstarter_sh_next_pids = <<1,2,3>>
        /\ vmstarter_sh_exited_pids = {}
        /\ machine_running = TRUE
        /\ vmshutdown_exited = FALSE
        (* Process respawn_vmstart *)
        /\ respawn_current_pid = NULL
        /\ pc = [self \in ProcSet |-> CASE self = "init" -> "init_"
                                        [] self = "respawn_vmstart" -> "init_r"
                                        [] self = "vmstarter_sh" -> "init_v"
                                        [] self = "vmshutdown" -> "init_vm"]

init_ == /\ pc["init"] = "init_"
         /\ IF ~shutdown_signal_received
               THEN /\ \/ /\ shutdown_signal_received' = TRUE
                       \/ /\ TRUE
                          /\ UNCHANGED shutdown_signal_received
                    /\ pc' = [pc EXCEPT !["init"] = "init_"]
               ELSE /\ pc' = [pc EXCEPT !["init"] = "wait_for_vmshutdown"]
                    /\ UNCHANGED shutdown_signal_received
         /\ UNCHANGED << start_allowed, start_allowed_locked,
                         vmstarter_sh_running, vmstarter_sh_spawn_pending,
                         vmstarter_sh_sigterm_pending, vmstarter_sh_next_pids,
                         vmstarter_sh_exited_pids, machine_running,
                         vmshutdown_exited, respawn_current_pid >>

wait_for_vmshutdown == /\ pc["init"] = "wait_for_vmshutdown"
                       /\ vmshutdown_exited
                       /\ pc' = [pc EXCEPT !["init"] = "poweroff_to_kernel"]
                       /\ UNCHANGED << start_allowed, start_allowed_locked,
                                       shutdown_signal_received,
                                       vmstarter_sh_running,
                                       vmstarter_sh_spawn_pending,
                                       vmstarter_sh_sigterm_pending,
                                       vmstarter_sh_next_pids,
                                       vmstarter_sh_exited_pids,
                                       machine_running, vmshutdown_exited,
                                       respawn_current_pid >>

poweroff_to_kernel == /\ pc["init"] = "poweroff_to_kernel"
                      /\ machine_running' = FALSE
                      /\ pc' = [pc EXCEPT !["init"] = "Done"]
                      /\ UNCHANGED << start_allowed, start_allowed_locked,
                                      shutdown_signal_received,
                                      vmstarter_sh_running,
                                      vmstarter_sh_spawn_pending,
                                      vmstarter_sh_sigterm_pending,
                                      vmstarter_sh_next_pids,
                                      vmstarter_sh_exited_pids,
                                      vmshutdown_exited, respawn_current_pid >>

init == init_ \/ wait_for_vmshutdown \/ poweroff_to_kernel

init_r == /\ pc["respawn_vmstart"] = "init_r"
          /\ IF ~shutdown_signal_received
                THEN /\ pc' = [pc EXCEPT !["respawn_vmstart"] = "respawn_flock_enter"]
                ELSE /\ pc' = [pc EXCEPT !["respawn_vmstart"] = "halt_"]
          /\ UNCHANGED << start_allowed, start_allowed_locked,
                          shutdown_signal_received, vmstarter_sh_running,
                          vmstarter_sh_spawn_pending,
                          vmstarter_sh_sigterm_pending, vmstarter_sh_next_pids,
                          vmstarter_sh_exited_pids, machine_running,
                          vmshutdown_exited, respawn_current_pid >>

respawn_flock_enter == /\ pc["respawn_vmstart"] = "respawn_flock_enter"
                       /\ start_allowed_locked = FALSE
                       /\ start_allowed_locked' = TRUE
                       /\ pc' = [pc EXCEPT !["respawn_vmstart"] = "respawn_check_start_allowed"]
                       /\ UNCHANGED << start_allowed, shutdown_signal_received,
                                       vmstarter_sh_running,
                                       vmstarter_sh_spawn_pending,
                                       vmstarter_sh_sigterm_pending,
                                       vmstarter_sh_next_pids,
                                       vmstarter_sh_exited_pids,
                                       machine_running, vmshutdown_exited,
                                       respawn_current_pid >>

respawn_check_start_allowed == /\ pc["respawn_vmstart"] = "respawn_check_start_allowed"
                               /\ IF start_allowed
                                     THEN /\ pc' = [pc EXCEPT !["respawn_vmstart"] = "respawn_vmstarter_spawn"]
                                     ELSE /\ pc' = [pc EXCEPT !["respawn_vmstart"] = "respawn_not_allowed"]
                               /\ UNCHANGED << start_allowed,
                                               start_allowed_locked,
                                               shutdown_signal_received,
                                               vmstarter_sh_running,
                                               vmstarter_sh_spawn_pending,
                                               vmstarter_sh_sigterm_pending,
                                               vmstarter_sh_next_pids,
                                               vmstarter_sh_exited_pids,
                                               machine_running,
                                               vmshutdown_exited,
                                               respawn_current_pid >>

respawn_vmstarter_spawn == /\ pc["respawn_vmstart"] = "respawn_vmstarter_spawn"
                           /\ vmstarter_sh_spawn_pending' = Head(vmstarter_sh_next_pids)
                           /\ respawn_current_pid' = vmstarter_sh_spawn_pending'
                           /\ vmstarter_sh_next_pids' = Tail(vmstarter_sh_next_pids)
                           /\ pc' = [pc EXCEPT !["respawn_vmstart"] = "respawn_vmstarter_wait"]
                           /\ UNCHANGED << start_allowed, start_allowed_locked,
                                           shutdown_signal_received,
                                           vmstarter_sh_running,
                                           vmstarter_sh_sigterm_pending,
                                           vmstarter_sh_exited_pids,
                                           machine_running, vmshutdown_exited >>

respawn_vmstarter_wait == /\ pc["respawn_vmstart"] = "respawn_vmstarter_wait"
                          /\ ~machine_running \/ respawn_current_pid \in vmstarter_sh_exited_pids
                          /\ IF ~machine_running
                                THEN /\ pc' = [pc EXCEPT !["respawn_vmstart"] = "halt_"]
                                ELSE /\ pc' = [pc EXCEPT !["respawn_vmstart"] = "respawn_flock_exit"]
                          /\ UNCHANGED << start_allowed, start_allowed_locked,
                                          shutdown_signal_received,
                                          vmstarter_sh_running,
                                          vmstarter_sh_spawn_pending,
                                          vmstarter_sh_sigterm_pending,
                                          vmstarter_sh_next_pids,
                                          vmstarter_sh_exited_pids,
                                          machine_running, vmshutdown_exited,
                                          respawn_current_pid >>

respawn_not_allowed == /\ pc["respawn_vmstart"] = "respawn_not_allowed"
                       /\ TRUE
                       /\ pc' = [pc EXCEPT !["respawn_vmstart"] = "respawn_flock_exit"]
                       /\ UNCHANGED << start_allowed, start_allowed_locked,
                                       shutdown_signal_received,
                                       vmstarter_sh_running,
                                       vmstarter_sh_spawn_pending,
                                       vmstarter_sh_sigterm_pending,
                                       vmstarter_sh_next_pids,
                                       vmstarter_sh_exited_pids,
                                       machine_running, vmshutdown_exited,
                                       respawn_current_pid >>

respawn_flock_exit == /\ pc["respawn_vmstart"] = "respawn_flock_exit"
                      /\ start_allowed_locked' = FALSE
                      /\ pc' = [pc EXCEPT !["respawn_vmstart"] = "init_r"]
                      /\ UNCHANGED << start_allowed, shutdown_signal_received,
                                      vmstarter_sh_running,
                                      vmstarter_sh_spawn_pending,
                                      vmstarter_sh_sigterm_pending,
                                      vmstarter_sh_next_pids,
                                      vmstarter_sh_exited_pids,
                                      machine_running, vmshutdown_exited,
                                      respawn_current_pid >>

halt_ == /\ pc["respawn_vmstart"] = "halt_"
         /\ TRUE
         /\ pc' = [pc EXCEPT !["respawn_vmstart"] = "Done"]
         /\ UNCHANGED << start_allowed, start_allowed_locked,
                         shutdown_signal_received, vmstarter_sh_running,
                         vmstarter_sh_spawn_pending,
                         vmstarter_sh_sigterm_pending, vmstarter_sh_next_pids,
                         vmstarter_sh_exited_pids, machine_running,
                         vmshutdown_exited, respawn_current_pid >>

respawn_vmstart == init_r \/ respawn_flock_enter
                      \/ respawn_check_start_allowed
                      \/ respawn_vmstarter_spawn \/ respawn_vmstarter_wait
                      \/ respawn_not_allowed \/ respawn_flock_exit \/ halt_

init_v == /\ pc["vmstarter_sh"] = "init_v"
          /\ IF machine_running
                THEN /\ pc' = [pc EXCEPT !["vmstarter_sh"] = "vmstarter_sh_wait_to_be_launched"]
                ELSE /\ pc' = [pc EXCEPT !["vmstarter_sh"] = "halt"]
          /\ UNCHANGED << start_allowed, start_allowed_locked,
                          shutdown_signal_received, vmstarter_sh_running,
                          vmstarter_sh_spawn_pending,
                          vmstarter_sh_sigterm_pending, vmstarter_sh_next_pids,
                          vmstarter_sh_exited_pids, machine_running,
                          vmshutdown_exited, respawn_current_pid >>

vmstarter_sh_wait_to_be_launched == /\ pc["vmstarter_sh"] = "vmstarter_sh_wait_to_be_launched"
                                    /\ ~machine_running \/ vmstarter_sh_spawn_pending /= NULL
                                    /\ IF ~machine_running
                                          THEN /\ pc' = [pc EXCEPT !["vmstarter_sh"] = "halt"]
                                               /\ UNCHANGED << vmstarter_sh_running,
                                                               vmstarter_sh_spawn_pending >>
                                          ELSE /\ vmstarter_sh_running' = vmstarter_sh_spawn_pending
                                               /\ vmstarter_sh_spawn_pending' = NULL
                                               /\ pc' = [pc EXCEPT !["vmstarter_sh"] = "vmstarter_await_shutdown_or_crash"]
                                    /\ UNCHANGED << start_allowed,
                                                    start_allowed_locked,
                                                    shutdown_signal_received,
                                                    vmstarter_sh_sigterm_pending,
                                                    vmstarter_sh_next_pids,
                                                    vmstarter_sh_exited_pids,
                                                    machine_running,
                                                    vmshutdown_exited,
                                                    respawn_current_pid >>

vmstarter_await_shutdown_or_crash == /\ pc["vmstarter_sh"] = "vmstarter_await_shutdown_or_crash"
                                     /\ IF Len(vmstarter_sh_next_pids) > 0
                                           THEN /\ \/ /\ vmstarter_sh_sigterm_pending = vmstarter_sh_running
                                                   \/ /\ TRUE
                                           ELSE /\ vmstarter_sh_sigterm_pending = vmstarter_sh_running
                                     /\ vmstarter_sh_exited_pids' = (vmstarter_sh_exited_pids \union {vmstarter_sh_running})
                                     /\ vmstarter_sh_running' = NULL
                                     /\ pc' = [pc EXCEPT !["vmstarter_sh"] = "init_v"]
                                     /\ UNCHANGED << start_allowed,
                                                     start_allowed_locked,
                                                     shutdown_signal_received,
                                                     vmstarter_sh_spawn_pending,
                                                     vmstarter_sh_sigterm_pending,
                                                     vmstarter_sh_next_pids,
                                                     machine_running,
                                                     vmshutdown_exited,
                                                     respawn_current_pid >>

halt == /\ pc["vmstarter_sh"] = "halt"
        /\ TRUE
        /\ pc' = [pc EXCEPT !["vmstarter_sh"] = "Done"]
        /\ UNCHANGED << start_allowed, start_allowed_locked,
                        shutdown_signal_received, vmstarter_sh_running,
                        vmstarter_sh_spawn_pending,
                        vmstarter_sh_sigterm_pending, vmstarter_sh_next_pids,
                        vmstarter_sh_exited_pids, machine_running,
                        vmshutdown_exited, respawn_current_pid >>

vmstarter_sh == init_v \/ vmstarter_sh_wait_to_be_launched
                   \/ vmstarter_await_shutdown_or_crash \/ halt

init_vm == /\ pc["vmshutdown"] = "init_vm"
           /\ shutdown_signal_received
           /\ pc' = [pc EXCEPT !["vmshutdown"] = "vmshutdown_inhibit_new_starts"]
           /\ UNCHANGED << start_allowed, start_allowed_locked,
                           shutdown_signal_received, vmstarter_sh_running,
                           vmstarter_sh_spawn_pending,
                           vmstarter_sh_sigterm_pending,
                           vmstarter_sh_next_pids, vmstarter_sh_exited_pids,
                           machine_running, vmshutdown_exited,
                           respawn_current_pid >>

vmshutdown_inhibit_new_starts == /\ pc["vmshutdown"] = "vmshutdown_inhibit_new_starts"
                                 /\ start_allowed' = FALSE
                                 /\ pc' = [pc EXCEPT !["vmshutdown"] = "vmshutdown_check_running_command"]
                                 /\ UNCHANGED << start_allowed_locked,
                                                 shutdown_signal_received,
                                                 vmstarter_sh_running,
                                                 vmstarter_sh_spawn_pending,
                                                 vmstarter_sh_sigterm_pending,
                                                 vmstarter_sh_next_pids,
                                                 vmstarter_sh_exited_pids,
                                                 machine_running,
                                                 vmshutdown_exited,
                                                 respawn_current_pid >>

vmshutdown_check_running_command == /\ pc["vmshutdown"] = "vmshutdown_check_running_command"
                                    /\ IF start_allowed_locked = TRUE
                                          THEN /\ pc' = [pc EXCEPT !["vmshutdown"] = "vmshutdown_try_signal"]
                                          ELSE /\ pc' = [pc EXCEPT !["vmshutdown"] = "vmshutdown_done"]
                                    /\ UNCHANGED << start_allowed,
                                                    start_allowed_locked,
                                                    shutdown_signal_received,
                                                    vmstarter_sh_running,
                                                    vmstarter_sh_spawn_pending,
                                                    vmstarter_sh_sigterm_pending,
                                                    vmstarter_sh_next_pids,
                                                    vmstarter_sh_exited_pids,
                                                    machine_running,
                                                    vmshutdown_exited,
                                                    respawn_current_pid >>

vmshutdown_try_signal == /\ pc["vmshutdown"] = "vmshutdown_try_signal"
                         /\ IF vmstarter_sh_running /= NULL
                               THEN /\ vmstarter_sh_sigterm_pending' = vmstarter_sh_running
                               ELSE /\ TRUE
                                    /\ UNCHANGED vmstarter_sh_sigterm_pending
                         /\ pc' = [pc EXCEPT !["vmshutdown"] = "vmshutdown_check_running_command"]
                         /\ UNCHANGED << start_allowed, start_allowed_locked,
                                         shutdown_signal_received,
                                         vmstarter_sh_running,
                                         vmstarter_sh_spawn_pending,
                                         vmstarter_sh_next_pids,
                                         vmstarter_sh_exited_pids,
                                         machine_running, vmshutdown_exited,
                                         respawn_current_pid >>

vmshutdown_done == /\ pc["vmshutdown"] = "vmshutdown_done"
                   /\ vmshutdown_exited' = TRUE
                   /\ pc' = [pc EXCEPT !["vmshutdown"] = "Done"]
                   /\ UNCHANGED << start_allowed, start_allowed_locked,
                                   shutdown_signal_received,
                                   vmstarter_sh_running,
                                   vmstarter_sh_spawn_pending,
                                   vmstarter_sh_sigterm_pending,
                                   vmstarter_sh_next_pids,
                                   vmstarter_sh_exited_pids, machine_running,
                                   respawn_current_pid >>

vmshutdown == init_vm \/ vmshutdown_inhibit_new_starts
                 \/ vmshutdown_check_running_command
                 \/ vmshutdown_try_signal \/ vmshutdown_done

(* Allow infinite stuttering to prevent deadlock on termination. *)
Terminating == /\ \A self \in ProcSet: pc[self] = "Done"
               /\ UNCHANGED vars

Next == init \/ respawn_vmstart \/ vmstarter_sh \/ vmshutdown
           \/ Terminating

Spec == /\ Init /\ [][Next]_vars
        /\ WF_vars(init)
        /\ WF_vars(respawn_vmstart)
        /\ WF_vars(vmstarter_sh)
        /\ WF_vars(vmshutdown)

Termination == <>(\A self \in ProcSet: pc[self] = "Done")

\* END TRANSLATION

\* TEMPORAL PROPERTIES:
\* If we signal ACPI shutdown, vmstart eventually stops running and never restarts
ShutdownSignalWorks == (shutdown_signal_received ~> ([](vmstarter_sh_running = NULL)))
\* Before we signal ACPI shutdown, respawn works
RespawnBeforeShutdownCanRestartWithoutPendingShutdown == TRUE \* TODO: how to express this?

=============================================================================
\* Modification History
\* Last modified Mon Sep 25 11:11:48 CEST 2023 by cs
\* Created Sun Sep 24 12:17:50 CEST 2023 by cs
