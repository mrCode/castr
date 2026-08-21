# What a Chromecast actually requires

Getting a live desktop onto a Chromecast took seven changes. Six of them fail
*silently* -- the receiver accepts the request, fetches video, and then does
nothing, with no error anywhere. This is the record of what each one looked
like from the sending end, so the next person recognises the symptom instead of
rediscovering the cause.

Measured against a Xiaomi TV Stick (`MiTV-AESP0`, "Cinema"), receiver
`CrKey/1.56.500000 DeviceType/AndroidTV`, using the Default Media Receiver
(`CC1AD845`).

## 1. A live stream must be HLS

A single endless response does not work. Both an endless fragmented MP4 and an
endless WebM were accepted at LOAD; the receiver fetched a few hundred
kilobytes and then stopped reading, permanently, reporting no player state.

Live video has to arrive as segments behind a playlist the receiver re-reads.

## 2. CORS headers are required, and their absence is invisible

This is the one that costs a day.

Progressive video -- a plain MP4 file -- is played by a `<video>` element and
needs no CORS. It works immediately, which is misleading. HLS is played through
Media Source Extensions, where the receiver *is a web page* fetching each
playlist and segment, and a response without `Access-Control-Allow-Origin` is
delivered to the browser and then discarded before the page sees it.

From the server, a healthy request log:

```
playlist -> 200, 164 bytes, 2 segments listed
playlist -> 200, 164 bytes, 2 segments listed
playlist -> 200, 164 bytes, 2 segments listed
```

Three or four playlist reads, every one a success, **not one segment request**,
and no player state ever. Nothing is an error. Adding the header made the same
build fetch segments on the next run.

## 3. The stream must have an audio track, even a silent one

A video-only HLS stream is refused with `LOAD_FAILED` and `idleReason: ERROR`
-- *after* the receiver has downloaded two segments, which makes it look like a
video decoding problem. It is not. Muxing in `audiotestsrc wave=silence`
through an AAC encoder made the same video play, unchanged.

## 4. `application/vnd.apple.mpegurl`, not `application/x-mpegurl`

The RFC 8216 type. ffmpeg says so out loud -- "mime type is not rfc8216
compliant" -- which is how it was found.

## 5. `EXT-X-TARGETDURATION` must cover every `EXTINF`

`hlssink2` writes the target duration it was *asked* for, while `EXTINF`
reports what each segment actually came out as. They differ, because segments
end on a keyframe and the capture's real framerate is below the nominal one: a
one-second target produced 1.2-second segments, and a playlist that violates
the spec. castr corrects the value as the playlist is served rather than
raising the target, because segment length is the floor on how far behind the
television runs.

## 6. The capture needs a fixed framerate, not a maximum

The portal emits a frame when the screen changes and nothing when it does not.
On a still desktop that is under two frames a second -- measured
`avg_frame_rate=5/3`. For a segmented live stream it is fatal: the receiver
drains the playlist faster than the encoder fills it, plays a few seconds and
stalls.

A fixed rate makes `videorate` repeat the last frame, which costs almost
nothing to encode. The capsfilter carrying it must sit *after* `videorate`; on
the source pad it makes `pipewiresrc` fail negotiation ("set output format:
-22") and produce no buffers at all, silently.

## 7. The first reply to LOAD is not the verdict

The receiver answers a LOAD immediately with a `MEDIA_STATUS` quoting the
request id and a player state of `IDLE`, and only later sends `LOAD_FAILED` or
a status showing `BUFFERING`/`PLAYING`.

Matching on the request id and returning reports every failed cast as a
success. It did, for four rounds of testing here, while the television sat on
its home screen. castr waits for the verdict.

## What success looks like

```
t+ 5s  served=4001 KB  requests=15  playlist-reads=6   player=PLAYING
t+30s  served=17350 KB requests=65  playlist-reads=30  player=PLAYING
t+60s  served=32752 KB requests=125 playlist-reads=60  player=PLAYING
```

The number that matters is **playlist-reads climbing**. A receiver that is
following a live stream re-reads the playlist every segment. One that has given
up reads it a few times and stops -- while reporting nothing wrong at all.
