// Command gawk-mumblesim is a scripted native Mumble user for the e2e tests
// (docs/80 MV7): it logs in to murmur with the bridge's own client, joins a
// channel, and either echoes every voice packet it hears as its own voice
// (-echo) or plays a fixture (-play). Once a second it prints one JSON line
// to stdout with the frames it received per speaker name and its own
// session, so a test can assert both directions. Logged in as SuperUser it
// can also kick or server-mute a user, create the temporary-channel parent
// and grant an access token MakeTempChannel on it (the docs/80 §5 ACL).
//
//	gawk-mumblesim fixture < tone.ogg > tone.opus   (testdata/gen-tone.sh)
//
// It never writes audio anywhere but back to murmur.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/Tuhis/gawk/gawk-mumble-bridge/internal/mumble"
	"github.com/Tuhis/gawk/gawk-mumble-bridge/internal/opusfixture"
	"github.com/Tuhis/gawk/gawk-server/voice"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// Report is the once-a-second JSON line.
type Report struct {
	Time     int64            `json:"t"`
	Name     string           `json:"name"`
	Session  uint32           `json:"session"`
	Channel  uint32           `json:"channel"`
	Received map[string]int   `json:"received"`
	Ends     map[string]int   `json:"ends"`
	Sent     int              `json:"sent"`
	Users    map[string]uint8 `json:"users"`
}

type options struct {
	addr, name, password, token, channel, tempUnder, tempName string
	insecure, echo, loop                                      bool
	play, kick, mute, unmute, grantTemp                       string
	duration                                                  time.Duration
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "fixture" {
		ps, err := opusfixture.FromOgg(stdin)
		if err == nil {
			err = opusfixture.Write(stdout, ps)
		}
		if err != nil {
			fmt.Fprintln(stderr, "gawk-mumblesim fixture:", err)
			return 1
		}
		return 0
	}
	var o options
	fs := flag.NewFlagSet("gawk-mumblesim", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.addr, "addr", "127.0.0.1:64738", "murmur host:port")
	fs.StringVar(&o.name, "name", "native", "Mumble user name")
	fs.StringVar(&o.password, "password", "", "server password (or SuperUser's)")
	fs.StringVar(&o.token, "token", "", "access token")
	fs.BoolVar(&o.insecure, "insecure", false, "accept any murmur certificate")
	fs.StringVar(&o.channel, "channel", "", "channel path to join (Friends/Gaming); created if missing when SuperUser")
	fs.StringVar(&o.tempUnder, "temp-under", "", "parent path of a temporary channel to join (with -temp-name)")
	fs.StringVar(&o.tempName, "temp-name", "", "temporary channel name; or -temp-code to derive gawk's")
	tempCode := fs.String("temp-code", "", "room code: join gawk's temporary channel for it (with -ticket-secret)")
	secret := fs.String("ticket-secret", "", "the bridge's -ticket-secret, for -temp-code")
	tmpl := fs.String("temp-template", voice.DefaultTempChannelTemplate, "the bridge's -mumble-temp-channel-template, for -temp-code")
	fs.BoolVar(&o.echo, "echo", false, "re-send every Opus packet heard as this user's voice")
	fs.StringVar(&o.play, "play", "", "play a fixture of length-prefixed 10 ms Opus packets")
	fs.BoolVar(&o.loop, "loop", false, "with -play: repeat")
	fs.StringVar(&o.kick, "kick", "", "kick the user with this name (needs SuperUser)")
	fs.StringVar(&o.mute, "mute", "", "server-mute the user with this name (needs SuperUser)")
	fs.StringVar(&o.unmute, "unmute", "", "server-unmute the user with this name")
	fs.StringVar(&o.grantTemp, "grant-temp", "", "with -token: grant #token MakeTempChannel and Enter on -channel (needs SuperUser)")
	fs.DurationVar(&o.duration, "duration", 0, "exit after this long; 0 runs until SIGTERM")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *tempCode != "" {
		if *secret == "" {
			fmt.Fprintln(stderr, "gawk-mumblesim: -temp-code needs -ticket-secret")
			return 2
		}
		o.tempName = voice.TempChannelName(*tmpl, []byte(*secret), *tempCode, *tempCode)
	}
	if o.duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.duration)
		defer cancel()
	}
	if err := sim(ctx, o, stdout); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		fmt.Fprintln(stderr, "gawk-mumblesim:", err)
		return 1
	}
	return 0
}

func sim(ctx context.Context, o options, stdout io.Writer) error {
	var fixture [][]byte
	if o.play != "" {
		f, err := os.Open(o.play)
		if err != nil {
			return err
		}
		fixture, err = opusfixture.Read(f)
		f.Close()
		if err != nil {
			return err
		}
	}
	var tokens []string
	if o.token != "" {
		tokens = []string{o.token}
	}
	c, err := mumble.Dial(ctx, mumble.Config{Addr: o.addr, Username: o.name, Password: o.password, Tokens: tokens,
		TLS: mumble.TLSOptions{InsecureSkipVerify: o.insecure}, AudioQueue: 1024, Release: "gawk-mumblesim"})
	if err != nil {
		return err
	}
	defer c.Close()
	if o.channel != "" {
		id, err := ensurePath(ctx, c, voice.ChannelPath(o.channel))
		if err != nil {
			return fmt.Errorf("channel %s: %w", o.channel, err)
		}
		if o.grantTemp != "" {
			if err := c.SetACL(id, []mumble.ACLEntry{{Group: "#" + o.grantTemp, ApplyHere: true, ApplySubs: true, Grant: mumble.PermMakeTempChannel | mumble.PermEnter | mumble.PermTraverse | mumble.PermSpeak}}); err != nil {
				return err
			}
		}
		if o.tempName == "" {
			if err := c.JoinChannel(ctx, id); err != nil {
				return err
			}
		}
	}
	if o.tempName != "" {
		parent, ok := c.FindPath(voice.ChannelPath(o.tempUnder))
		if !ok {
			return fmt.Errorf("temporary parent %q: %w", o.tempUnder, mumble.ErrChannelNotFound)
		}
		if _, err := c.CreateChannel(ctx, parent, o.tempName, true); err != nil {
			return err
		}
	}
	for _, a := range []struct {
		name string
		do   func(uint32) error
	}{
		{o.kick, func(s uint32) error { return c.Kick(s, "gawk-mumblesim", false) }},
		{o.mute, func(s uint32) error { return c.SetServerMute(s, true) }},
		{o.unmute, func(s uint32) error { return c.SetServerMute(s, false) }},
	} {
		if a.name == "" {
			continue
		}
		found := false
		for _, u := range c.Users() {
			if u.Name == a.name {
				found = true
				if err := a.do(u.Session); err != nil {
					return err
				}
			}
		}
		if !found {
			return fmt.Errorf("no user named %q", a.name)
		}
	}

	var mu sync.Mutex
	rep := Report{Name: o.name, Received: map[string]int{}, Ends: map[string]int{}}
	go func() {
		var seq int64
		for {
			p, err := c.Audio().Pop(ctx)
			if err != nil {
				return
			}
			name := fmt.Sprint(p.Session)
			if u, ok := c.User(p.Session); ok {
				name = u.Name
			}
			mu.Lock()
			rep.Received[name]++
			if p.End {
				rep.Ends[name]++
			}
			mu.Unlock()
			if o.echo {
				if ok, _ := c.WriteAudio(seq, p.Opus, p.End); ok {
					mu.Lock()
					rep.Sent++
					mu.Unlock()
				}
				seq++
			}
		}
	}()
	if fixture != nil {
		go func() {
			t := time.NewTicker(10 * time.Millisecond)
			defer t.Stop()
			var seq int64
			for {
				for i, p := range fixture {
					select {
					case <-t.C:
					case <-ctx.Done():
						return
					}
					if ok, _ := c.WriteAudio(seq, p, i == len(fixture)-1); ok {
						mu.Lock()
						rep.Sent++
						mu.Unlock()
					}
					seq++
				}
				if !o.loop {
					return
				}
			}
		}()
	}
	enc := json.NewEncoder(stdout)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	emit := func() {
		self := c.Self()
		users := map[string]uint8{}
		for _, u := range c.UsersIn(self.Channel) {
			var f uint8
			if u.Mute {
				f |= 1
			}
			if u.SelfMute {
				f |= 2
			}
			users[u.Name] = f
		}
		mu.Lock()
		rep.Time, rep.Session, rep.Channel, rep.Users = time.Now().Unix(), self.Session, self.Channel, users
		_ = enc.Encode(rep)
		mu.Unlock()
	}
	for {
		select {
		case <-tick.C:
			emit()
		case <-c.Done():
			emit()
			return c.Err()
		case <-ctx.Done():
			emit()
			return ctx.Err()
		}
	}
}

// ensurePath resolves a channel path, creating missing permanent channels
// (which only works with MakeChannel, i.e. as SuperUser).
func ensurePath(ctx context.Context, c *mumble.Client, path []string) (uint32, error) {
	id := uint32(0)
	for _, name := range path {
		next, ok := c.FindChild(id, name)
		if !ok {
			var err error
			if next, err = c.CreateChannel(ctx, id, name, false); err != nil {
				return 0, err
			}
		}
		id = next
	}
	return id, nil
}
