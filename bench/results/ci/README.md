# CI checks run locally

Logs of local runs of the CI's make targets, kept because the docs cite them.

## Fuzzing stalls (make ci-fuzz)

In the local `make ci-fuzz` runs at 12c8a01, two of the eight targets stopped
executing after a few seconds of their 10 s: FuzzParseVideo at 3,832 execs and
FuzzFECRecovery at 44,243, against 309,207-329,858 for the other six. Both ran with
the default `-fuzzminimizetime` (60 s per input).

`fuzz_parsevideo_default_minimize.log` is FuzzParseVideo for 15 s with
`GODEBUG=fuzzdebug=1` and the default: executions stop at 23,941 once two inputs are
queued for minimization, and the minimization is still running when the 15 s are up.
`fuzz_parsevideo_minimize_100x.log` is the same with `-fuzzminimizetime 100x`: each
minimization takes 2-8 ms, ends with "interesting input that doesn't expand coverage"
(the new coverage did not reproduce), and the target runs 495,949 execs in 15 s.
`fuzz_fecrecovery_minimize_100x.log`: FuzzFECRecovery with 100x, 474,006 execs in 15 s.
`make ci-fuzz` now passes `-fuzzminimizetime 100x`, as `make fuzz` already did.

Commands (server/, nice 19, CPU 0, under the bench lock):

    GOMAXPROCS=2 GODEBUG=fuzzdebug=1 go test -run '^$' -fuzz '^FuzzParseVideo$' -fuzztime 15s -parallel 2 ./internal/protocol
    GOMAXPROCS=2 GODEBUG=fuzzdebug=1 go test -run '^$' -fuzz '^FuzzParseVideo$' -fuzztime 15s -fuzzminimizetime 100x -parallel 2 ./internal/protocol
    GOMAXPROCS=2 go test -run '^$' -fuzz '^FuzzFECRecovery$' -fuzztime 15s -fuzzminimizetime 100x -parallel 2 ./internal/protocol
