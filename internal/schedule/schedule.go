// Package schedule runs servers' scheduled tasks: lists of steps (restart,
// update, back up, send a console command, wait, …) run at set times, such
// as a nightly restart that warns players first.
package schedule

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Kinds of schedule.
const (
	WhenDaily = "daily" // at a time, on chosen weekdays
	WhenEvery = "every" // every so many minutes
	WhenCron  = "cron"  // a cron expression
)

// Kinds of step.
const (
	StepStart   = "start"
	StepStop    = "stop"
	StepRestart = "restart"
	StepUpdate  = "update"  // only if an update is available
	StepBackup  = "backup"  // the server's backup plan, waiting for it
	StepCommand = "command" // through the server's console
	StepWait    = "wait"
	// StepIfPlayers carries on only if players are (or aren't) online;
	// otherwise the task stops, skipped.
	StepIfPlayers = "if_players"
)

var stepKinds = []string{StepStart, StepStop, StepRestart, StepUpdate, StepBackup, StepCommand, StepWait, StepIfPlayers}

// Conditions of an if_players step.
const (
	PlayersNone = "none" // carry on only if nobody's online
	PlayersSome = "some" // carry on only if someone is
)

// Limits.
const (
	maxTasks    = 20
	maxSteps    = 20
	maxWait     = 3600 // seconds
	minEvery    = 5    // minutes
	maxEvery    = 7 * 24 * 60
	keepRuns    = 5
	maxNameLen  = 64
	maxCommand  = 1024
	missedAfter = 15 * time.Minute // later than this after hlm was down, a run is skipped
	busyGiveUp  = 10 * time.Minute // how long a run waits for the server to be free
)

// When says when a task runs.
type When struct {
	Kind string `json:"kind"`
	// Time ("04:00") and Weekdays (0 is Sunday; empty means every day) are
	// for daily tasks.
	Time     string `json:"time,omitempty"`
	Weekdays []int  `json:"weekdays,omitempty"`
	// Every is the interval in minutes, for every tasks.
	Every int `json:"every,omitempty"`
	// Cron is a five-field cron expression, for cron tasks.
	Cron string `json:"cron,omitempty"`
}

// Step is one thing a task does.
type Step struct {
	Kind string `json:"kind"`
	// Command is the console command, for command steps.
	Command string `json:"command,omitempty"`
	// Seconds is how long to wait, for wait steps.
	Seconds int `json:"seconds,omitempty"`
	// Players is the condition of an if_players step: none or some.
	Players string `json:"players,omitempty"`
	// ContinueOnError carries on with the next step if this one fails;
	// otherwise the task stops.
	ContinueOnError bool `json:"continueOnError,omitempty"`
}

// Task is a server's scheduled task.
type Task struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	When    When   `json:"when"`
	Steps   []Step `json:"steps"`
	// OnlyIfRunning skips the task when the server is stopped, so a nightly
	// restart doesn't start a server someone stopped.
	OnlyIfRunning bool `json:"onlyIfRunning"`
	// Next is when it next runs; nil when it's turned off.
	Next *time.Time `json:"next,omitempty"`
	// Anchor is when an every task's intervals count from.
	Anchor time.Time `json:"anchor,omitzero"`
	// Runs are the latest runs, newest first.
	Runs []Run `json:"runs,omitempty"`
}

// Results of a run.
const (
	ResultOK      = "ok"
	ResultFailed  = "failed"
	ResultSkipped = "skipped" // e.g. the server was stopped
	ResultMissed  = "missed"  // hlm wasn't running at the time
)

// Run is one run of a task.
type Run struct {
	Trigger  string       `json:"trigger"` // schedule or manual
	Started  time.Time    `json:"started"`
	Finished time.Time    `json:"finished"`
	Result   string       `json:"result"`
	Error    string       `json:"error,omitempty"`
	Steps    []StepResult `json:"steps,omitempty"`
}

// StepResult is how one step went.
type StepResult struct {
	Kind    string `json:"kind"`
	OK      bool   `json:"ok"`
	Skipped bool   `json:"skipped,omitempty"`
	Message string `json:"message,omitempty"`
}

func parseClock(s string) (int, int, bool) {
	h, m, ok := strings.Cut(s, ":")
	if !ok || len(h) == 0 || len(h) > 2 || len(m) != 2 {
		return 0, 0, false
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, 0, false
	}
	return hh, mm, true
}

// cron returns the schedule as a cron expression, for daily and cron
// tasks.
func (w When) cron() (Cron, error) {
	switch w.Kind {
	case WhenDaily:
		h, m, ok := parseClock(w.Time)
		if !ok {
			return Cron{}, errors.New("the time must look like 04:30")
		}
		days := "*"
		if len(w.Weekdays) > 0 && len(w.Weekdays) < 7 {
			var ds []string
			for _, d := range w.Weekdays {
				ds = append(ds, strconv.Itoa(d))
			}
			days = strings.Join(ds, ",")
		}
		return ParseCron(fmt.Sprintf("%d %d * * %s", m, h, days))
	case WhenCron:
		return ParseCron(w.Cron)
	}
	return Cron{}, fmt.Errorf("no cron form for %q", w.Kind)
}

// next returns when a task with this schedule runs next after after.
func (w When) next(after, anchor time.Time, loc *time.Location) time.Time {
	if w.Kind == WhenEvery {
		d := time.Duration(w.Every) * time.Minute
		if d <= 0 {
			return time.Time{}
		}
		if anchor.IsZero() || anchor.After(after) {
			anchor = after
		}
		n := after.Sub(anchor)/d + 1
		return anchor.Add(n * d)
	}
	c, err := w.cron()
	if err != nil {
		return time.Time{}
	}
	return c.Next(after, loc)
}

func (w When) check() (When, error) {
	switch w.Kind {
	case WhenDaily:
		w.Every, w.Cron = 0, ""
		var days []int
		for _, d := range w.Weekdays {
			if d < 0 || d > 6 {
				return w, errors.New("weekdays are 0 (Sunday) to 6 (Saturday)")
			}
			if !slices.Contains(days, d) {
				days = append(days, d)
			}
		}
		slices.Sort(days)
		if len(days) == 7 {
			days = nil
		}
		w.Weekdays = days
		w.Time = strings.TrimSpace(w.Time)
		h, m, ok := parseClock(w.Time)
		if !ok {
			return w, errors.New("the time must look like 04:30")
		}
		w.Time = fmt.Sprintf("%02d:%02d", h, m)
	case WhenEvery:
		w.Time, w.Weekdays, w.Cron = "", nil, ""
		if w.Every < minEvery || w.Every > maxEvery {
			return w, fmt.Errorf("the interval must be between %d minutes and a week", minEvery)
		}
	case WhenCron:
		w.Time, w.Weekdays, w.Every = "", nil, 0
		w.Cron = strings.TrimSpace(w.Cron)
		c, err := ParseCron(w.Cron)
		if err != nil {
			return w, err
		}
		if c.Next(time.Now(), time.UTC).IsZero() {
			return w, errors.New("that cron expression never matches")
		}
	default:
		return w, fmt.Errorf("unknown kind of schedule %q", w.Kind)
	}
	return w, nil
}

// check validates a task as sent by the API.
func (t Task) check() (Task, error) {
	t.Name = strings.TrimSpace(t.Name)
	if t.Name == "" {
		return t, errors.New("give the task a name")
	}
	if len(t.Name) > maxNameLen {
		return t, fmt.Errorf("the name can be at most %d characters", maxNameLen)
	}
	w, err := t.When.check()
	if err != nil {
		return t, err
	}
	t.When = w
	if len(t.Steps) == 0 {
		return t, errors.New("add at least one step")
	}
	if len(t.Steps) > maxSteps {
		return t, fmt.Errorf("a task can have at most %d steps", maxSteps)
	}
	steps := make([]Step, len(t.Steps))
	for i, st := range t.Steps {
		if !slices.Contains(stepKinds, st.Kind) {
			return t, fmt.Errorf("step %d: unknown kind %q", i+1, st.Kind)
		}
		clean := Step{Kind: st.Kind, ContinueOnError: st.ContinueOnError}
		switch st.Kind {
		case StepCommand:
			clean.Command = strings.TrimSpace(st.Command)
			if clean.Command == "" {
				return t, fmt.Errorf("step %d: enter the command", i+1)
			}
			if len(clean.Command) > maxCommand {
				return t, fmt.Errorf("step %d: the command can be at most %d characters", i+1, maxCommand)
			}
		case StepIfPlayers:
			if st.Players != PlayersNone && st.Players != PlayersSome {
				return t, fmt.Errorf("step %d: choose whether players have to be online or not", i+1)
			}
			clean.Players = st.Players
			clean.ContinueOnError = false
		case StepWait:
			if st.Seconds < 1 || st.Seconds > maxWait {
				return t, fmt.Errorf("step %d: wait between 1 second and an hour", i+1)
			}
			clean.Seconds = st.Seconds
			clean.ContinueOnError = false
		}
		steps[i] = clean
	}
	t.Steps = steps
	return t, nil
}

// Describe says when a schedule runs, in words.
func (w When) Describe() string {
	switch w.Kind {
	case WhenDaily:
		if len(w.Weekdays) == 0 {
			return "every day at " + w.Time
		}
		names := []string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}
		var ds []string
		for _, d := range w.Weekdays {
			ds = append(ds, names[d])
		}
		return strings.Join(ds, ", ") + " at " + w.Time
	case WhenEvery:
		if w.Every%60 == 0 {
			if w.Every == 60 {
				return "every hour"
			}
			return fmt.Sprintf("every %d hours", w.Every/60)
		}
		return fmt.Sprintf("every %d minutes", w.Every)
	case WhenCron:
		return "cron " + w.Cron
	}
	return ""
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
