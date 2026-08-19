package main

import (
	"context"
	"fmt"
	"os"

	"github.com/flemzord/skillloop/cmd"
)

func main() {
	if err := cmd.Execute(context.Background()); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
