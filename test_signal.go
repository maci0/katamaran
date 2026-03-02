package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		fmt.Println("Context done, calling stop() to restore default behavior")
		stop()
	}()

	fmt.Println("Waiting for signal...")
	<-ctx.Done()
	fmt.Println("Signal received. Sleeping for 5 seconds...")
	time.Sleep(5 * time.Second)
	fmt.Println("Done sleeping.")
}
