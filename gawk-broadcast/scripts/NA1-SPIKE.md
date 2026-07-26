# NA1 audio spike — instructions for whoever has the Linux machine

You've been asked to run one script on a Linux desktop and send back a file.
It takes about **three minutes** and answers questions we cannot answer any other
way: we're adding system-audio capture to the native Linux broadcaster, and the
five facts it depends on are all "what does *this* machine's GStreamer and
PipeWire actually do" — unanswerable from a Mac, and not worth guessing when
the guesses become code.

You don't need to know anything about the project. You don't need the app.

## What it does to your machine

Nothing that outlives it:

- runs `gst-launch-1.0` pipelines (the GStreamer command-line tool)
- writes files into **one output directory** it creates in the current folder
- plays a quiet 440 Hz test tone through your speakers for ~40 s, so there is
  something for it to record

It does **not** use sudo, install anything, change any setting, touch the
network, or capture your screen — the share/permission dialog never appears,
because the video side of the test is a generated test pattern, not your
desktop. Pass `--dry-run` first if you want to see every command it will run
without running any of them.

It does record your **system audio output** for a few seconds at a time — that's
the whole point — so if you'd rather it not capture whatever you're listening
to, close it first. The test tone is enough on its own.

## 1. Check you have GStreamer

```sh
gst-launch-1.0 --version
```

If that prints a version, skip to step 2. Otherwise:

```sh
# Debian / Ubuntu / Pop / Mint
sudo apt install gstreamer1.0-tools gstreamer1.0-plugins-base \
  gstreamer1.0-plugins-good gstreamer1.0-plugins-bad gstreamer1.0-pipewire

# Fedora
sudo dnf install gstreamer1 gstreamer1-plugins-base gstreamer1-plugins-good \
  gstreamer1-plugins-bad-free gstreamer1-plugin-pipewire

# Arch
sudo pacman -S gstreamer gst-plugins-base gst-plugins-good gst-plugins-bad \
  gst-plugin-pipewire
```

`python3` is used for the analysis. It's almost certainly already there; if it
isn't, the script still collects everything and we'll analyse it on our end.

## 2. Run it

```sh
chmod +x na1-audio-spike.sh
./na1-audio-spike.sh
```

Leave it alone for a couple of minutes. You'll hear a quiet tone; that's expected.
It prints a summary and ends with the path of a `.tar.gz`.

**Something must actually be coming out of the speakers** for the whole first
half to mean anything — a capture of silence and a capture from the wrong device
look identical. The test tone handles that for you, so the simple advice is to
let it run. If you'd rather use your own audio, `./na1-audio-spike.sh --no-tone`
works, but then **start music playing first and leave it playing**; a `--no-tone`
run with nothing playing is the one way to come back with no answer.

## 3. The one-minute manual extra (worth it)

```sh
./na1-audio-spike.sh --follow-test
```

Same as above, plus a 24-second recording at the end during which you should
**switch your default output device** — speakers to headphones, or plug/unplug
headphones, or pick a different output in Sound settings. Do it about 8 seconds
in; the script tells you when.

This is the one behaviour we most want to know about: whether audio capture
survives you changing outputs mid-broadcast, or dies silently. The script
reports the recording's level in four time slices, so "went quiet halfway
through" is visible rather than averaged away.

## 4. Send back

Send **the `.tar.gz`** — it has everything. If that's awkward, paste the
contents of `SUMMARY.txt` (inside the output directory) into chat instead; it's
written to be readable and is roughly one screen.

## If it goes wrong

Send the tarball anyway, or `console.log` from the output directory. A failed
run is a result — "candidate 1 doesn't work on Fedora 42" is exactly the kind
of thing this exists to find out, and it changes what we build. There is no way
to run this "wrong" short of not running it.

Two known-harmless things: several probes are *expected* to fail (we test
several ways of doing the same thing on purpose, to find out which one this
machine likes), and `timeout` kills each pipeline deliberately — a non-zero exit
code in the logs usually means "we stopped it", not "it broke".
