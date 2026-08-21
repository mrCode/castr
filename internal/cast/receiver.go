package cast

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// App is an application running on a receiver.
type App struct {
	AppID string `json:"appId"`
	// SessionID identifies this run of the app. LOAD is refused without it.
	SessionID string `json:"sessionId"`
	// TransportID is the address to talk to the app itself. Messages for the
	// app go here, not to receiver-0, and the app needs its own CONNECT.
	TransportID string `json:"transportId"`
	DisplayName string `json:"displayName"`
	StatusText  string `json:"statusText"`
}

type receiverStatus struct {
	Type   string `json:"type"`
	Reason string `json:"reason"`
	Status struct {
		Applications []App `json:"applications"`
	} `json:"status"`
}

// Launch starts an app and returns it once the receiver reports it running.
//
// An app that is already running is returned as it stands rather than
// restarted: relaunching drops whatever is playing, and the common case for
// hitting this is a second cast to a receiver already showing the first.
func (c *Conn) Launch(ctx context.Context, appID string) (App, error) {
	if app, err := c.runningApp(ctx, appID); err == nil {
		return app, nil
	}

	raw, err := c.request(ctx, platformID, nsReceiver, map[string]any{
		"type": "LAUNCH", "appId": appID,
	})
	if err != nil {
		return App{}, fmt.Errorf("launching %s: %w", appID, err)
	}

	var status receiverStatus
	if err := json.Unmarshal(raw, &status); err != nil {
		return App{}, fmt.Errorf("launching %s: unreadable reply: %w", appID, err)
	}
	if status.Type == "LAUNCH_ERROR" {
		reason := status.Reason
		if reason == "" {
			reason = "no reason given"
		}
		return App{}, fmt.Errorf("the receiver refused to launch %s: %s", appID, reason)
	}
	for _, app := range status.Status.Applications {
		if app.AppID == appID {
			return app, nil
		}
	}
	return App{}, fmt.Errorf("the receiver reported no %s after launching it", appID)
}

func (c *Conn) runningApp(ctx context.Context, appID string) (App, error) {
	raw, err := c.request(ctx, platformID, nsReceiver, map[string]any{"type": "GET_STATUS"})
	if err != nil {
		return App{}, err
	}
	var status receiverStatus
	if err := json.Unmarshal(raw, &status); err != nil {
		return App{}, err
	}
	for _, app := range status.Status.Applications {
		if app.AppID == appID && app.TransportID != "" {
			return app, nil
		}
	}
	return App{}, fmt.Errorf("%s is not running", appID)
}

// Media describes what to play.
type Media struct {
	// URL is fetched by the RECEIVER, not by this machine. It has to name an
	// address the receiver can reach: a link-local or loopback address here
	// produces a receiver that reports a load failure with no explanation.
	URL string
	// ContentType is what the receiver uses to pick a player. Its media
	// support is narrower than a desktop browser's.
	ContentType string
	Title       string
	// Live tells the receiver there is no seeking and no duration. Without it
	// the player waits for a duration that a live capture never supplies.
	Live bool
}

type mediaStatus struct {
	Type          string `json:"type"`
	DetailedError any    `json:"detailedErrorCode"`
	Status        []struct {
		PlayerState string `json:"playerState"`
	} `json:"status"`
}

// Load asks a running app to play something.
func (c *Conn) Load(ctx context.Context, app App, m Media) error {
	// The app is a separate endpoint from the receiver and needs its own
	// CONNECT. Skipping it makes LOAD vanish silently -- no error, no reply.
	if err := c.send(Message{
		Source: senderID, Destination: app.TransportID,
		Namespace: nsConnection, Payload: `{"type":"CONNECT"}`,
	}); err != nil {
		return fmt.Errorf("connecting to the receiver's player: %w", err)
	}

	streamType := "BUFFERED"
	if m.Live {
		streamType = "LIVE"
	}
	// Registered BEFORE the request is sent, and synchronously. The receiver
	// can answer faster than this goroutine would get back to a subscription,
	// and a verdict that arrives before the watcher exists is lost.
	waitForVerdict, cancel, err := c.addWatcher(loadVerdict)
	if err != nil {
		return err
	}
	defer cancel()

	raw, err := c.request(ctx, app.TransportID, nsMedia, map[string]any{
		"type":      "LOAD",
		"sessionId": app.SessionID,
		"autoplay":  true,
		"media": map[string]any{
			"contentId":   m.URL,
			"contentType": m.ContentType,
			"streamType":  streamType,
			"metadata": map[string]any{
				"metadataType": 0,
				"title":        m.Title,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("asking the receiver to play the stream: %w", err)
	}
	_ = raw

	// Waiting for the verdict rather than for the reply. A receiver answers a
	// LOAD immediately with a MEDIA_STATUS quoting the request id and a player
	// state of IDLE, and only afterwards says whether it could play the
	// stream. Treating that first reply as the answer reports every failed
	// cast as a success -- which it did, for four rounds of testing against
	// hardware, while the television sat on its home screen.
	if err := waitForVerdict(ctx); err != nil {
		return fmt.Errorf("%w. The receiver must be able to reach %s and to play %s",
			err, m.URL, m.ContentType)
	}
	return nil
}

// loadVerdict reads media-namespace messages until the receiver has actually
// committed to playing, or refused.
func loadVerdict(namespace, payload string, done func(error)) bool {
	if namespace != nsMedia {
		return false
	}
	var status struct {
		Type          string `json:"type"`
		DetailedError any    `json:"detailedErrorCode"`
		Status        []struct {
			PlayerState string `json:"playerState"`
			IdleReason  string `json:"idleReason"`
		} `json:"status"`
	}
	if json.Unmarshal([]byte(payload), &status) != nil {
		return false
	}

	switch status.Type {
	case "LOAD_FAILED", "LOAD_CANCELLED", "INVALID_REQUEST":
		done(fmt.Errorf("the receiver refused the stream (%s%s)",
			status.Type, detail(status.DetailedError)))
		return true
	case "MEDIA_STATUS":
		for _, s := range status.Status {
			switch s.PlayerState {
			case "PLAYING", "BUFFERING":
				done(nil)
				return true
			case "IDLE":
				if s.IdleReason == "ERROR" {
					done(errors.New("the receiver could not play the stream " +
						"(it went idle with an error)"))
					return true
				}
			}
		}
	}
	return false
}

func detail(code any) string {
	if code == nil || code == "" {
		return ""
	}
	return fmt.Sprintf(", error %v", code)
}

// Stop closes the app running on the receiver, returning it to its home screen.
func (c *Conn) Stop(ctx context.Context, app App) error {
	_, err := c.request(ctx, platformID, nsReceiver, map[string]any{
		"type": "STOP", "sessionId": app.SessionID,
	})
	if err != nil {
		return fmt.Errorf("stopping %s: %w", app.AppID, err)
	}
	return nil
}

// PlayerState is what the receiver says it is doing: PLAYING, BUFFERING,
// PAUSED, IDLE, or empty when no media is loaded at all.
//
// This is the difference between "the television accepted the stream" and "the
// television is showing it". LOAD returning without an error means only that
// the request was well formed; a receiver that cannot decode the stream
// accepts the LOAD and then sits in BUFFERING forever.
func (c *Conn) PlayerState(ctx context.Context, app App) (string, error) {
	raw, err := c.request(ctx, app.TransportID, nsMedia, map[string]any{
		"type": "GET_STATUS",
	})
	if err != nil {
		return "", err
	}
	var status mediaStatus
	if err := json.Unmarshal(raw, &status); err != nil {
		return "", fmt.Errorf("unreadable media status: %w", err)
	}
	if len(status.Status) == 0 {
		return "", nil
	}
	return status.Status[0].PlayerState, nil
}
