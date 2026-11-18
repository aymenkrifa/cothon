package engine

import (
	"context"
	"os/exec"
	"time"
)

func notifySend(title, body string, critical bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	argv := osascriptArgv(title, body, critical)
	exec.CommandContext(ctx, argv[0], argv[1:]...).Run()
}
