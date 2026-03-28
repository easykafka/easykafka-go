When running unit test TestShutdownEngineStopSignal in github actions pipeline we are getting following error:

=== Failed
=== FAIL: tests/unit TestShutdownEngineStopSignal (0.20s)
==================
WARNING: DATA RACE
Write at 0x00c0001950e8 by goroutine 96:
github.com/easykafka/easykafka-go/internal/engine.(*Engine).Stop()
/home/runner/work/easykafka-go/easykafka-go/internal/engine/engine.go:343 +0x6cf
github.com/easykafka/easykafka-go/tests/unit.TestShutdownEngineStopSignal()
/home/runner/work/easykafka-go/easykafka-go/tests/unit/shutdown_test.go:298 +0x6c1
testing.tRunner()
/opt/hostedtoolcache/go/1.24.13/x64/src/testing/testing.go:1792 +0x225
testing.(*T).Run.gowrap1()
/opt/hostedtoolcache/go/1.24.13/x64/src/testing/testing.go:1851 +0x44

Previous read at 0x00c0001950e8 by goroutine 97:
github.com/easykafka/easykafka-go/internal/engine.(*Engine).runSingleLoop()
/home/runner/work/easykafka-go/easykafka-go/internal/engine/engine.go:141 +0x5b
github.com/easykafka/easykafka-go/internal/engine.(*Engine).Start()
/home/runner/work/easykafka-go/easykafka-go/internal/engine/engine.go:120 +0x364
github.com/easykafka/easykafka-go/tests/unit.TestShutdownEngineStopSignal.func2()
/home/runner/work/easykafka-go/easykafka-go/tests/unit/shutdown_test.go:291 +0x4f

Goroutine 96 (running) created at:
testing.(*T).Run()
/opt/hostedtoolcache/go/1.24.13/x64/src/testing/testing.go:1851 +0x8f2
testing.runTests.func1()
/opt/hostedtoolcache/go/1.24.13/x64/src/testing/testing.go:2279 +0x85
testing.tRunner()
/opt/hostedtoolcache/go/1.24.13/x64/src/testing/testing.go:1792 +0x225
testing.runTests()
/opt/hostedtoolcache/go/1.24.13/x64/src/testing/testing.go:2277 +0x96c
testing.(*M).Run()
/opt/hostedtoolcache/go/1.24.13/x64/src/testing/testing.go:2142 +0xeea
main.main()
_testmain.go:267 +0x164

Goroutine 97 (running) created at:
github.com/easykafka/easykafka-go/tests/unit.TestShutdownEngineStopSignal()
/home/runner/work/easykafka-go/easykafka-go/tests/unit/shutdown_test.go:290 +0x604
testing.tRunner()
/opt/hostedtoolcache/go/1.24.13/x64/src/testing/testing.go:1792 +0x225
testing.(*T).Run.gowrap1()
/opt/hostedtoolcache/go/1.24.13/x64/src/testing/testing.go:1851 +0x44
==================
testing.go:1490: race detected during execution of test

DONE 132 tests, 1 failure in 23.383s


Please do the following:

* find the root cause
* explain it in detail
* suggest solution for it