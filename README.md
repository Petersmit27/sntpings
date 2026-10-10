# pings

Tools for drawing on the [SNTpings](https://pings.utwente.io/) canvas: a shared
3840×2160 screen where every ICMPv6 ping is one pixel. Each packet goes to a
unique IPv6 address inside the event's /64 prefix, and that address encodes the
pixel's position and colour. The receiver never replies.

| Tool | What it does |
| --- | --- |
| `pingd` + `pingctl` | Keep a whole scene of images (including animated GIFs) on the canvas from one or more servers, edited live from your laptop. |
| `sntping` | Draw one image from the command line, with optional effects. No servers or setup needed. |

## Address format

```
<PREFIX /64>:<X>:<Y>:<B><G>:<R><A>
```

All fields are hexadecimal: `X` and `Y` are the 16-bit pixel coordinates
(0–3839, 0–2159); `B`, `G`, `R`, `A` are the 8-bit blue, green, red and alpha
channels. Pixel (25,25) in `#FFD100` at full opacity, with prefix
`2001:db8:b317:a000::/64`, becomes:

```
2001:0db8:b317:a000:0019:0019:00d1:ffff
```

`sntping -dry` prints exactly what a run would send, without sending anything:

```sh
./sntping -image ffd100.png -prefix 2001:db8:b317:a000::/64 -fit none -x 25 -y 25 -dry
# 2001:0db8:b317:a000:0019:0019:00d1:ffff
```

## Requirements

- Linux with IPv6 connectivity and a route to the event prefix
  (`ip -6 route get <PREFIX>::1`).
- No root needed for sending: the tools use unprivileged ping sockets (`udp6`).
  The kernel allows them when `net.ipv4.ping_group_range` includes your group.
  Most distros default to `0 2147483647` (everyone); if
  `cat /proc/sys/net/ipv4/ping_group_range` says `1 0`, enable it with
  `sudo sysctl -w net.ipv4.ping_group_range="0 2147483647"`.
- Go 1.27 or newer to build.
- For `pingd`: servers with systemd, rsync and root ssh access from your laptop.

Supported images: PNG, JPEG, GIF (animated GIFs play in `pingd`; `sntping` uses
the first frame), BMP, TIFF and WebP. Pixels with an alpha below 16 are skipped,
so transparent areas cost no packets.

## Building

```sh
go build -o pingctl ./cmd/pingctl
go build -o sntping .
```

To run `sntping` on another machine, build a static binary that needs nothing
installed there and copy it across:

```sh
CGO_ENABLED=0 go build -ldflags "-s -w" -o sntping .
```

`pingctl deploy` builds `pingd` itself.

## A scene on several servers: pingd + pingctl

`pingd` runs on each server as a systemd service and keeps every image of its
`scene.json` on the canvas. Images are composited first, so overlaps are sent
once and later images are drawn on top. It notices changes to the scene or its
images within a quarter of a second, and writes the current rate to
`status.json`.

`pingctl` runs on your laptop, from this directory. Your local `scene.json` is
the source of truth: every change is saved there and pushed to all servers.

### Setup

1. Put your servers in the `hosts` list at the top of `cmd/pingctl/main.go`, or
   set `PINGCTL_HOSTS=root@a.example,root@b.example`. Check the event prefix
   (`prefix`) in the same file.
2. Build and install:

   ```sh
   go build -o pingctl ./cmd/pingctl
   ./pingctl deploy        # builds pingd and (re)installs it on every server
   ```

   `deploy` uploads the binary, scene and images, installs `pingd.service` and
   (re)starts it. To deploy to one new server only, so the others keep running:
   `PINGCTL_HOSTS=root@new.example ./pingctl deploy`.

### Commands

```sh
./pingctl add logo.png -x 25 -y 80 -scale 0.4 -nearest   # upload and add on top
./pingctl add party.gif -scale 2 -nearest -speed 0.5
./pingctl set logo -x 30 -scale 0.5 -top
./pingctl set party -frame-ms 600
./pingctl rm logo
./pingctl list                                            # the scene, with sizes and GIF timing
./pingctl status                                          # rate and share per server
./pingctl push                                            # upload scene and images again
./pingctl edit                                            # live keyboard control
```

`x` and `y` are percentages of the free space (canvas minus image): 0 is flush
left/top, 100 flush right/bottom, 50 centred. An image always stays fully on
the canvas, whatever its size.

### Live editing

`pingctl edit` keeps one ssh connection per server open, so every key press is
on the canvas almost immediately. The screen shows the scene and each server's
rate.

| Keys | Action |
| --- | --- |
| Tab / Shift-Tab, 1–9 | select an image |
| Arrows | move 1% |
| Shift + arrows | move 10% |
| Ctrl + arrows (or Alt + arrows) | move 0.1% |
| `+` / `-` | scale up / down |
| `[` / `]` | scale −0.01 / +0.01 |
| `,` / `.` (or `<` / `>`) | GIF slower / faster |
| `n` | nearest-neighbour scaling on/off |
| `t` | bring to top |
| `x` `x` | remove (press twice) |
| `q` | quit; the scene keeps running |

### Animated GIFs

The frame on show is picked from the wall clock, so every server sends the same
frame at the same moment. `-speed S` plays a GIF at S times its own speed
(0.5 = half speed) and keeps its rhythm; `-frame-ms N` shows every frame for N
ms instead and overrides `-speed`.

The picture changes at most every 250 ms, so shorter frames are skipped (at
real-time speed). Slow a GIF down when its frames are shorter than a pass
(pixels per server ÷ packets per second), otherwise the canvas never finishes
a frame and shows a blend. Every frame change gives pixels new colours, and so
new addresses, which costs more than repeating a still image: big, fast GIFs
lower the packet rate.

### Splitting the scene across servers

Every server sends only its own share of the pixels, chosen by position, so
together they cover the scene once instead of each sending all of it. A pixel
always belongs to the same server, whatever its colour. `pingctl` writes the
server list into the scene on every push, and each `pingd` finds its own
hostname in it; a server that is not listed sends the whole scene.

Keep each server's share small: the packet rate drops as the number of
distinct pixels per server grows.

If a server goes down, its share stops being refreshed. Push without it to
re-split over the others, and push again once it is back:

```sh
PINGCTL_HOSTS=root@a.example,root@b.example ./pingctl push
```

### On a server

```sh
journalctl -u pingd -f        # placement, errors, and the rate every minute
cat /root/pings/status.json   # current rate, share, pixels per pass
```

## sntping: draw one image

A single command for when you don't want to run `pingd`: it draws one image
from the machine you start it on.

```sh
./sntping -image logo.png -prefix <PREFIX>          # one pass
./sntping -image logo.png -prefix <PREFIX> -loop    # keep redrawing
./sntping -image logo.png -prefix <PREFIX> -dry     # print addresses only
```

Stop with Ctrl-C; final stats are printed on exit.

### Sizing and placement

By default the image is scaled to fit the 4K canvas (change the canvas with
`-w` / `-h`) and centred.

| Flags | Effect |
| --- | --- |
| `-fit fit` | scale to fit, keeping the aspect ratio (default) |
| `-fit stretch` | stretch to fill the whole canvas |
| `-fit none` | original pixel size |
| `-scale 0.5` | resize by a factor (overrides `-fit`) |
| `-rw 800` / `-rh 400` | force a width or height, keeping the aspect ratio; both together force an exact size |
| `-nearest` | nearest-neighbour resizing: crisp pixels, best for emotes and pixel art (default is smooth Catmull-Rom) |
| `-x 100 -y 50` | top-left corner at pixel (100,50) |
| `-x 25% -y 80%` | percentage of the free space, as in `pingctl`; pixels and percentages can be mixed |

```sh
./sntping -image logo.png -prefix <PREFIX> -fit none                         # centred, original size
./sntping -image logo.png -prefix <PREFIX> -scale 0.5 -x 0 -y 0              # half size, top left
./sntping -image emote.png -prefix <PREFIX> -scale 4 -nearest -x 100% -y 100%
```

### Live positioning

`-live` lets you steer the image while it runs, then prints the flags to run it
with again:

```sh
./sntping -image logo.png -prefix <PREFIX> -scale 2 -live
```

Arrows move 1% of the free space, Shift + arrows 10%, `+` / `-` scale, and `q`
quits, printing e.g. `rerun with : -x 56% -y 45% -scale 2.25`. `-live` implies
`-loop`, reads keys from a terminal or a pipe (end of input quits), and can't be
combined with `-dvd`, `-kitchen-sink` or `-dry`.

### Speed

Packets go out as fast as the machine can. `-pps N` sets a ceiling (a token
bucket shared by all workers); `-workers N` sets the number of sender sockets
(default 2× CPU cores).

### Order and effects

| Flags | Effect |
| --- | --- |
| `-shuffle` | send pixels in random order instead of scan lines (`-seed N` for a fixed order) |
| `-kitchen-sink` | shuffle, loop, and drift the image to a new spot every pass |
| `-dvd` | bounce the image around the canvas like a DVD screensaver (`-dvd-speed N` pixels per pass, default 4) |
| `-hue N` | sweep the colours N degrees around the colour wheel per pass; `-hue 90 -dvd` is very obvious |
| `-interval 2s` | pause between passes when looping |

### Flag reference

```
-image path      image to draw (PNG/JPEG/GIF/BMP/TIFF/WebP) [required]
-prefix addr     IPv6 /64 prefix to ping, e.g. 2001:db8:b317:a000:: [required]
-w int           canvas width in pixels (default 3840)
-h int           canvas height in pixels (default 2160)
-fit string      fit | stretch | none (default "fit")
-scale float     resize factor, e.g. 0.5 or 2 (overrides -fit)
-rw int          resize to this width in pixels (overrides -scale)
-rh int          resize to this height in pixels (overrides -fit)
-x pos           left edge: pixels or N% of the free space (default: centred)
-y pos           top edge: pixels or N% of the free space (default: centred)
-nearest         resize with nearest-neighbour instead of Catmull-Rom
-live            steer with arrows and +/- while running (implies -loop)
-min-alpha int   skip pixels with alpha below this (0-255, default 16)
-workers int     sender sockets (default 2x CPU cores)
-pps int         packets-per-second ceiling (default 0 = unlimited)
-shuffle         send pixels in random order instead of scan lines
-seed int        random seed for -shuffle (0 = time-based)
-kitchen-sink    shuffle + loop + drifting image
-dvd             bounce the image around, DVD screensaver style (implies -loop)
-dvd-speed int   pixels the image moves per pass with -dvd (default 4)
-hue float       sweep the hue N degrees per pass (implies -loop)
-loop            keep redrawing until interrupted
-interval dur    pause between passes with -loop (default 0)
-dry             print destination addresses, send nothing
-max int         max packets per pass (0 = all), handy for testing
-bind addr       local IPv6 address to bind to (default "::")
-socket string   udp6 (unprivileged) or ip6:ipv6-icmp (needs CAP_NET_RAW)
```
