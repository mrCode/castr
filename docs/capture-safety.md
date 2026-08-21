# Why castr verifies what it is capturing

omarchy-cast shipped a Chromecast backend that was observed streaming the
user's **webcam** to a television while reporting that it was mirroring the
screen. It was disabled rather than fixed, with this note:

> A cast that might broadcast the user's camera is not a feature that can ship
> behind a warning.

That backend is being carried forward into castr. This document is the
measurement that made it safe to do so. Everything below was reproduced on
this machine, on GStreamer 1.28.6, against a live portal session.

## The root cause

omarchy-cast built its source as:

```
pipewiresrc target-object=<portal node id> autoconnect=false fd=<fd>
```

`target-object` does not take a node id. `gst-inspect-1.0 pipewiresrc` says:

```
path           : The source path to connect to        (deprecated)
target-object  : The source name/serial to connect to
```

A node id matches neither a `node.name` nor an `object.serial`, so the source
never bound to the granted node. It then fell back to the default video
device.

The surviving comment in that file blames `path` for being "deprecated in
GStreamer 1.28 and silently ignored". That is not what happened: `path` is
deprecated but fully functional, and the defect was passing the right value to
the wrong property.

## What each configuration actually does

One portal session, granted node 76, five source configurations, seven seconds
each. "Linked to" is sampled repeatedly during the run from `pw-dump`, not once
at the end -- a single sample lands before the link exists and reads as clean.

| source configuration | captured | linked to |
|---|---|---|
| `path=76 autoconnect=false` | 0 B | nothing |
| `path=76 autoconnect=true` | 366 KB | node 76, the portal |
| `path=999999 autoconnect=false` | 0 B | nothing |
| `path=999999 autoconnect=true` | 1.5 MB | **the webcam** |
| `target-object=<serial of 76> autoconnect=true` | 368 KB | node 76, the portal |
| `target-object=88888888 autoconnect=true` | 1.5 MB | **the webcam** |

Three things follow.

**`autoconnect=false` is not a safety feature.** It was reached for as one --
it does stop the substitution -- but it stops all capture with it, including
the correct case. A source that captures nothing is not a source that failed
safe; it is a source that cannot be used.

**Every configuration that works can also substitute a device.** There is no
value of these properties that captures the granted node and refuses
everything else. The fallback is the element's behaviour, not a misconfiguration
we can spell our way out of.

**Therefore the pipeline string cannot be the safety mechanism.** It is chosen
for correctness -- `target-object=<object.serial>`, which works and is not
deprecated -- and the guarantee is enforced somewhere it can actually be
checked.

## The guarantee

After the pipeline starts, castr reads the PipeWire graph and confirms the
source feeding the encoder is the node the portal granted. A link to any other
node -- or no link at all within the timeout -- tears the session down and
reports a failure. The check runs for every cast, on every backend that
captures the screen itself.

The one property worth stating plainly: castr never streams a source it has not
identified. A cast that cannot prove what it is capturing does not run.
