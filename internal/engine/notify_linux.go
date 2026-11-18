package engine

import (
	"context"
	"os/exec"
	"time"
)

func notifySend(title, body string, critical bool) {
	urgency, icon := "normal", "emblem-ok-symbolic"
	if critical {
		urgency, icon = "critical", "dialog-error-symbolic"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// argv only; title/body come from logs and are passed as plain arguments.
	exec.CommandContext(ctx, "notify-send", "-a", "cothon", "-u", urgency, "-i", icon, "--", title, body).Run()
}
