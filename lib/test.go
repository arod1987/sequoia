package sequoia

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type Test struct {
	Actions []ActionSpec
	Flags   TestFlags
	Cm      *ContainerManager
}

type ActionSpec struct {
	Describe    string
	Image       string
	Command     string
	Wait        bool
	When        *WhenSpec
	Entrypoint  string
	Requires    string
	Concurrency string
	Duration    string
	Save        string
}

type WhenSpec struct {
	Eval     string
	Timeout  uint64
	Interval uint64
}

func ActionsFromFile(fileName string) []ActionSpec {
	var actions []ActionSpec
	ReadYamlFile(fileName, &actions)
	return actions
}

func ActionsFromArgs(image string, command string, wait bool) []ActionSpec {
	action := ActionSpec{
		Image:   image,
		Command: command,
		Wait:    wait,
	}
	return []ActionSpec{action}
}

func NewTest(flags TestFlags, cm *ContainerManager) Test {

	// define test actions from config and flags
	var actions []ActionSpec
	switch flags.Mode {
	case "image":
		actions = ActionsFromArgs(*flags.ImageName, *flags.ImageCommand, *flags.ImageWait)
	default:
		actions = ActionsFromFile(*flags.TestFile)
	}
	return Test{actions, flags, cm}
}

func (t *Test) Run(scope Scope) {

	// do optional setup
	if *t.Flags.SkipSetup == false {
		// if in default mode purge all containers
		if t.Flags.Mode == "" {
			t.Cm.RemoveAllContainers()
		}
		scope.Setup()
	} else if scope.Provider.GetType() != "docker" {
		// non-dynamic IP's need to be extrapolated before test
		scope.Provider.ProvideCouchbaseServers(scope.Spec.Servers)
		scope.InitCli()
	} else {
		// not doing setup but need to get cb versions
		scope.InitCli()
	}

	if *t.Flags.SkipTest == true {
		return
	}

	// run at least <repeat> times or forever if -1
	repeat := *t.Flags.Repeat
	loops := 0
	if repeat == -1 {
		// run forever
		for {
			t.runTest(scope, loops)
			loops++
		}
	} else {
		repeat++
		for loops = 0; loops < repeat; loops++ {
			t.runTest(scope, loops)
		}
	}
	t.Cm.TapHandle.AutoPlan()

	// do optional teardown
	if *t.Flags.SkipTeardown == false {
		scope.TearDown(*t.Flags.SoftTeardown)
	}
}

func (t *Test) runTest(scope Scope, loop int) {

	var lastAction ActionSpec
	scope.Aux = loop

	// run all actions in test
	for _, action := range t.Actions {

		if action.Image == "" {
			// reuse last action image
			action.Image = lastAction.Image

			// reuse last action requires
			if action.Requires == "" {
				action.Requires = lastAction.Requires
			}
			// reuse last duration
			if action.Duration == "" {
				action.Duration = lastAction.Duration
			}
			// reuse last concurrency
			if action.Concurrency == "" {
				action.Concurrency = lastAction.Concurrency
			}
		}

		// check action requirements
		if action.Requires != "" {
			ok := ParseTemplate(&scope, action.Requires)
			pass, err := strconv.ParseBool(ok)
			logerr(err)
			if pass == false {
				colorsay("skipping due to requirements: " + action.Requires)
				lastAction = action
				continue
			}
		}

		// resolve command
		command := scope.CompileCommand(action.Command)

		// resolve duration and concurrency
		var taskDuration time.Duration = 0
		var taskConcurrency = 0
		var err error
		if action.Duration != "" {
			// parse template if units not found
			if strings.Index(action.Duration, "ns") == -1 {
				action.Duration = fmt.Sprintf("%s%s", ParseTemplate(&scope, action.Duration), "ns")
			}
			taskDuration, err = time.ParseDuration(action.Duration)
			logerr(err)
		}
		if action.Concurrency != "" {
			action.Concurrency = ParseTemplate(&scope, action.Concurrency)
			taskConcurrency, err = strconv.Atoi(action.Concurrency)
			logerr(err)
		}

		if action.Describe == "" { // use command as describe
			action.Describe = fmt.Sprintf("start %s: %s", action.Image, strings.Join(command, " "))
		}

		// compile task
		task := ContainerTask{
			Name:        *t.Flags.ContainerName,
			Describe:    action.Describe,
			Image:       action.Image,
			Command:     command,
			Async:       !action.Wait,
			Duration:    taskDuration,
			Concurrency: taskConcurrency,
			LogLevel:    *t.Flags.LogLevel,
			LogDir:      *t.Flags.LogDir,
		}

		if scope.Provider.GetType() == "docker" {
			task.LinksTo = scope.Provider.(*DockerProvider).GetLinkPairs()
		}
		if action.Entrypoint != "" {
			task.Entrypoint = []string{action.Entrypoint}
		}

		// run task
		if task.Async == true {
			go t.runTask(&scope, task, action.When, action.Save)
		} else {
			t.runTask(&scope, task, action.When, action.Save)
		}

		lastAction = action
	}

	// kill test containers
	scope.Cm.RemoveManagedContainers(true)

}

func (t *Test) runTask(scope *Scope,
	task ContainerTask,
	actionWhen *WhenSpec,
	saveKey string) {

	// if command has 'when' then cannot start processing until ready
	if actionWhen != nil {
		var ready = false
		var err error
		var elapsed uint64 = 0
		for ready == false {
			when := ParseTemplate(scope, actionWhen.Eval)
			ready, err = strconv.ParseBool(when)
			logerr(err)
			interval := actionWhen.Interval
			if interval == 0 {
				interval = 1
			}
			time.Sleep(time.Duration(interval) * time.Second)
			elapsed += interval
			if actionWhen.Timeout > 0 &&
				elapsed > actionWhen.Timeout {
				// timeout
				ecolorsay("timed out waiting for: " + actionWhen.Eval)
				ready = true
			}
		}
	}

	// run
	cid := t.Cm.Run(task)
	if saveKey != "" {
		scope.Vars[saveKey] = cid
	}
}
