// smoke knocks on a door as an anonymous guest, runs one command in the
// shared shell and prints what came back. Dev tool for checking the app
// end to end: `go run ./tools/smoke -relay http://127.0.0.1:8787 -door k7f3x9q2 -code 4821KQ`.
// The host still has to click "Let them in".
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/idestis/knock/client"
	"github.com/idestis/knock/protocol"
)

func main() {
	relay := flag.String("relay", "http://127.0.0.1:8787", "relay URL")
	door := flag.String("door", "", "door id")
	code := flag.String("code", "", "code")
	cmd := flag.String("cmd", "echo knock-smoke-$((6*7))", "command to type")
	wait := flag.Duration("wait", 90*time.Second, "how long to wait for the host")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *wait+30*time.Second)
	defer cancel()
	c, err := client.Dial(ctx, *relay, protocol.Hello{Name: "Smoke Test", Device: "smoke", Client: "cli", Version: "smoke"})
	must(err)
	defer c.Close()
	hash, err := c.HashForDoor(ctx, *door, *code)
	must(err)
	var p protocol.KnockPending
	must(c.RequestInto(ctx, protocol.TypeKnockRequest, protocol.KnockRequest{DoorID: *door, CodeHash: hash}, &p))
	fmt.Println("knocked; waiting for the host to let us in")

	deadline := time.After(*wait)
	for {
		select {
		case e := <-c.Events:
			switch e.Type {
			case protocol.TypeSessionStart:
				fmt.Println("in. typing:", *cmd)
				time.Sleep(800 * time.Millisecond)
				must(c.WriteData(ctx, []byte(*cmd+"\r")))
				var out strings.Builder
				t := time.After(4 * time.Second)
			collect:
				for {
					select {
					case b := <-c.Data:
						out.Write(b)
					case <-t:
						break collect
					}
				}
				fmt.Printf("--- output ---\n%s\n--------------\n", out.String())
				_ = c.Send(ctx, protocol.TypeSessionEnd, nil)
				if strings.Contains(out.String(), "knock-smoke-42") {
					fmt.Println("PASS")
					return
				}
				fmt.Println("FAIL: command output not seen")
				os.Exit(1)
			case protocol.TypeError:
				var pe protocol.Error
				_ = e.Decode(&pe)
				fmt.Println("refused:", pe.Code, pe.Message)
				os.Exit(1)
			}
		case <-deadline:
			fmt.Println("timed out")
			os.Exit(1)
		}
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
