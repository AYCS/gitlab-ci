package executors

import (
	"fmt"
	"github.com/Sirupsen/logrus"
	"gitlab.com/gitlab-org/gitlab-ci-multi-runner/helpers"
)

func (e *AbstractExecutor) log() *logrus.Entry {
	return e.Config.Log().WithField("build", e.Build.ID)
}

func (e *AbstractExecutor) Debugln(args ...interface{}) {
	e.log().Debugln(args...)
}

func (e *AbstractExecutor) Println(args ...interface{}) {
	fmt.Fprintln(e.BuildLog, args...)

	if e.BuildLog.IsStdout() || len(args) == 0 {
		return
	}

	e.log().Println(args...)
}

func (e *AbstractExecutor) Infoln(args ...interface{}) {
	fmt.Fprint(e.BuildLog, helpers.ANSI_BOLD_GREEN+fmt.Sprintln(args...)+helpers.ANSI_RESET)

	if e.BuildLog.IsStdout() || len(args) == 0 {
		return
	}

	e.log().Println(args...)
}

func (e *AbstractExecutor) Warningln(args ...interface{}) {
	fmt.Fprint(e.BuildLog, helpers.ANSI_BOLD_YELLOW+"WARNING: "+fmt.Sprintln(args...)+helpers.ANSI_RESET)

	if e.BuildLog.IsStdout() {
		return
	}

	e.log().Warningln(args...)
}

func (e *AbstractExecutor) Errorln(args ...interface{}) {
	fmt.Fprint(e.BuildLog, helpers.ANSI_BOLD_RED+"ERROR: "+fmt.Sprintln(args...)+helpers.ANSI_RESET)

	if e.BuildLog.IsStdout() {
		return
	}

	e.log().Errorln(args...)
}
