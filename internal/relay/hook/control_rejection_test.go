package hook

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

func Test33ControlRejectionBeforeDispatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, server := net.Pipe()
	defer client.Close()
	done := make(chan error, 1)
	ownerState := t.TempDir()
	go func() {
		defer func() {
			if recover() != nil {
				done <- errors.New("control handler panic")
			}
		}()
		done <- HandleControl(ctx, server, ownerState)
	}()
	if err := client.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err := io.WriteString(client, "{\"protocol\":1,\"method\":\"not-a-command\",\"params\":{}}\n")
	if err != nil {
		t.Fatal(err)
	}
	row, err := readFrame(client)
	if err != nil {
		t.Fatal(err)
	}
	if len(row) != 2 || row.Get("protocol") != int64(1) || row.Get("requestRejected") != true {
		t.Fatal(row)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}
func Test33RejectionIsNotRefusalOrEOF(t *testing.T) {
	for _, tc := range []struct{ name, response, outcome string }{{"rejected", `{"protocol":1,"requestRejected":true}`, "guard_rejected_the_call"}, {"refused", `{"error":"refused","reason":"state_not_owned"}`, ""}, {"closed", "", "guard_said_nothing"}, {"wrong_protocol", `{"protocol":2,"requestRejected":true}`, ""}, {"extra_field", `{"protocol":1,"requestRejected":true,"decision":"block"}`, ""}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			client, server := controlPair(t)
			defer client.Close()
			done := make(chan error, 1)
			go func() {
				defer server.Close()
				defer func() {
					if recover() != nil {
						done <- errors.New("fake peer panic")
					}
				}()
				if _, err := readFrame(server); err != nil {
					done <- err
					return
				}
				if tc.response != "" {
					_, err := io.WriteString(server, tc.response+"\n")
					done <- err
				} else {
					done <- nil
				}
			}()
			result, err := RequestGuard(ctx, client, Object{}, GuardOptions{})
			if tc.outcome != "" {
				var response *responseError
				if !errors.As(err, &response) || response.outcome != tc.outcome {
					t.Fatal(result, err)
				}
			} else if err != nil {
				t.Fatal(err)
			} else if tc.name == "refused" && result.Get("error") != "refused" {
				t.Fatal(pyjson.Dumps(result, pyjson.Options{}))
			}
			if err = <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
