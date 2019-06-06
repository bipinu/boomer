package boomer

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/asaskevich/EventBus"
)

// Events is the global event bus instance.
var Events = EventBus.New()

var defaultBoomer = &Boomer{}

// Mode is the running mode of boomer, both standalone and distributed are supported.
type Mode int

const (
	// DistributedMode requires connecting to a master.
	DistributedMode Mode = iota
	// StandaloneMode will run without a master.
	StandaloneMode
)

// A Boomer is used to run tasks.
// This type is exposed, so users can create and control a Boomer instance programmatically.
type Boomer struct {
	masterHost string
	masterPort int

	hatchType   string
	mode        Mode
	rateLimiter RateLimiter
	slaveRunner *slaveRunner

	localRunner *localRunner
	hatchCount  int
	hatchRate   int
}

// NewBoomer returns a new Boomer.
func NewBoomer(masterHost string, masterPort int) *Boomer {
	return &Boomer{
		masterHost: masterHost,
		masterPort: masterPort,
		hatchType:  "asap",
		mode:       DistributedMode,
	}
}

// NewStandaloneBoomer returns a new Boomer, which can run without master.
func NewStandaloneBoomer(hatchCount int, hatchRate int) *Boomer {
	return &Boomer{
		hatchType:  "asap",
		hatchCount: hatchCount,
		hatchRate:  hatchRate,
		mode:       StandaloneMode,
	}
}

// SetRateLimiter allows user to use their own rate limiter.
// It must be called before the test is started.
func (b *Boomer) SetRateLimiter(rateLimiter RateLimiter) {
	b.rateLimiter = rateLimiter
}

// SetHatchType only accepts "asap" or "smooth".
// "asap" means spawning goroutines as soon as possible when the test is started.
// "smooth" means a constant pace.
func (b *Boomer) SetHatchType(hatchType string) {
	if hatchType != "asap" && hatchType != "smooth" {
		log.Printf("Wrong hatch-type, expected asap or smooth, was %s\n", hatchType)
		return
	}
	b.hatchType = hatchType
}

// SetMode only accepts boomer.DistributedMode and boomer.StandaloneMode.
func (b *Boomer) SetMode(mode Mode) {
	switch mode {
	case DistributedMode:
		b.mode = DistributedMode
	case StandaloneMode:
		b.mode = StandaloneMode
	default:
		log.Println("Invalid mode, ignored!")
	}
}

// AddOutput accepts outputs which implements the boomer.Output interface.
func (b *Boomer) AddOutput(o Output) {
	switch b.mode {
	case DistributedMode:
		b.slaveRunner.addOutput(o)
	case StandaloneMode:
		b.localRunner.addOutput(o)
	default:
		log.Println("Invalid mode, AddOutput ignored!")
	}
}

// Run accepts a slice of Task and connects to the locust master.
func (b *Boomer) Run(tasks ...*Task) {
	switch b.mode {
	case DistributedMode:
		b.slaveRunner = newSlaveRunner(b.masterHost, b.masterPort, tasks, b.rateLimiter, b.hatchType)
		b.slaveRunner.run()
	case StandaloneMode:
		b.localRunner = newLocalRunner(tasks, b.rateLimiter, b.hatchCount, b.hatchType, b.hatchRate)
		b.localRunner.run()
	default:
		log.Println("Invalid mode, expected boomer.DistributedMode or boomer.StandaloneMode")
	}
}

// RecordSuccess reports a success.
func (b *Boomer) RecordSuccess(requestType, name string, responseTime int64, responseLength int64) {
	if b.localRunner == nil && b.slaveRunner == nil {
		return
	}
	switch b.mode {
	case DistributedMode:
		b.slaveRunner.stats.requestSuccessChan <- &requestSuccess{
			requestType:    requestType,
			name:           name,
			responseTime:   responseTime,
			responseLength: responseLength,
		}
	case StandaloneMode:
		b.localRunner.stats.requestSuccessChan <- &requestSuccess{
			requestType:    requestType,
			name:           name,
			responseTime:   responseTime,
			responseLength: responseLength,
		}
	}
}

// RecordFailure reports a failure.
func (b *Boomer) RecordFailure(requestType, name string, responseTime int64, exception string) {
	if b.localRunner == nil && b.slaveRunner == nil {
		return
	}
	switch b.mode {
	case DistributedMode:
		b.slaveRunner.stats.requestFailureChan <- &requestFailure{
			requestType:  requestType,
			name:         name,
			responseTime: responseTime,
			error:        exception,
		}
	case StandaloneMode:
		b.localRunner.stats.requestFailureChan <- &requestFailure{
			requestType:  requestType,
			name:         name,
			responseTime: responseTime,
			error:        exception,
		}
	}
}

// Quit will send a quit message to the master.
func (b *Boomer) Quit() {
	Events.Publish("boomer:quit")
	var ticker = time.NewTicker(3 * time.Second)

	switch b.mode {
	case DistributedMode:
		// wait for quit message is sent to master
		select {
		case <-b.slaveRunner.client.disconnectedChannel():
			break
		case <-ticker.C:
			log.Println("Timeout waiting for sending quit message to master, boomer will quit any way.")
			break
		}
		b.slaveRunner.close()
	case StandaloneMode:
		b.localRunner.close()
	}
}

// Run tasks without connecting to the master.
func runTasksForTest(tasks ...*Task) {
	taskNames := strings.Split(runTasks, ",")
	for _, task := range tasks {
		if task.Name == "" {
			continue
		} else {
			for _, name := range taskNames {
				if name == task.Name {
					log.Println("Running " + task.Name)
					task.Fn()
				}
			}
		}
	}
}

// Run accepts a slice of Task and connects to a locust master.
// It's a convenience function to use the defaultBoomer.
func Run(tasks ...*Task) {
	if !flag.Parsed() {
		flag.Parse()
	}

	if runTasks != "" {
		runTasksForTest(tasks...)
		return
	}

	initLegacyEventHandlers()

	if memoryProfile != "" {
		StartMemoryProfile(memoryProfile, memoryProfileDuration)
	}

	if cpuProfile != "" {
		StartCPUProfile(cpuProfile, cpuProfileDuration)
	}

	rateLimiter, err := createRateLimiter(maxRPS, requestIncreaseRate)
	if err != nil {
		log.Fatalf("%v\n", err)
	}
	defaultBoomer.SetRateLimiter(rateLimiter)
	defaultBoomer.masterHost = masterHost
	defaultBoomer.masterPort = masterPort
	defaultBoomer.hatchType = hatchType

	defaultBoomer.Run(tasks...)

	quitByMe := false
	Events.Subscribe("boomer:quit", func() {
		if !quitByMe {
			log.Println("shut down")
			os.Exit(0)
		}
	})

	c := make(chan os.Signal)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)

	<-c
	quitByMe = true
	defaultBoomer.Quit()

	log.Println("shut down")
}

// RecordSuccess reports a success.
// It's a convenience function to use the defaultBoomer.
func RecordSuccess(requestType, name string, responseTime int64, responseLength int64) {
	defaultBoomer.RecordSuccess(requestType, name, responseTime, responseLength)
}

// RecordFailure reports a failure.
// It's a convenience function to use the defaultBoomer.
func RecordFailure(requestType, name string, responseTime int64, exception string) {
	defaultBoomer.RecordFailure(requestType, name, responseTime, exception)
}
