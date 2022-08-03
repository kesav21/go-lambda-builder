package log

// logger.
// 	Do(d.buildExecutable).
// 	OnStart("Building executable").
// 	OnFail("Failed to build executable").
// 	OnPass("Built executable")

// This would do
//     fmt.Printf("%s | Building executable.\n", d.folder)
//     timer := newTimer()
// d.logger.Start("Building executable")

// d.logger.Fail(err, "Failed to build executable")

// d.logger.Stop("Built executable")

import (
	"fmt"
	"time"
)

type Logger interface {
	Start(string, ...any)
	Stop(string, ...any)
	Fail(error, string, ...any)
}

type logger struct {
	folder string
	timer  func() string
}

func NewLogger(folder string) Logger {
	return &logger{folder, nil}
}

func (l *logger) Start(format string, a ...any) {
	fmt.Printf("%s | %s.\n", l.folder, fmt.Sprintf(format, a...))
	l.timer = newTimer()
}

func (l *logger) Stop(format string, a ...any) {
	if l.timer == nil {
		return
	}
	fmt.Printf("%s | %s. Took %s.\n", l.folder, fmt.Sprintf(format, a...), l.timer())
}

func (l *logger) Fail(err error, format string, a ...any) {
	fmt.Printf("%s | %s: %s.\n", l.folder, fmt.Sprintf(format, a...), err.Error())
}

// Returns a function that returns a string.
// Expects duration to be less than one hour.
//
//     fmt.Printf("%s | Doing something.\n", folder)
//     t := newTimer()
//     err = doSomething(folder)
//     if err != nil {
//         fmt.Printf("%s | Failed to do something: %s\n", folder, err.Error())
//         return
//     }
//     fmt.Printf("%s | Did something. Took %s.\n", folder, t())
//
func newTimer() func() string {
	startTime := time.Now()
	return func() string {
		duration := time.Now().Sub(startTime)
		minutes := int(duration.Minutes())
		seconds := int(duration.Seconds()) % 60
		if minutes == 0 {
			return fmt.Sprintf("%d seconds", seconds)
		}
		return fmt.Sprintf("%d minutes and %d seconds", minutes, seconds)
	}
}
