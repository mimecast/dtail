asciinema
=========

The animated Gifs you find in the DTail docs were created using:

* [asciinema](https://asciinema.org) 
* [asciicast2gif](https://github.com/asciinema/asciicast2gif)

On Fedora Linux 35.

## Installing prerequisites

On Fedora Linux 35 install `asciinema`:

```shell
% sudo dnf install -y asciinema
```

and `asciicast2gif` (for simplicity, the Docker image was used):

```shell
% docker pull asciinema/asciicast2gif
```

This of course assumes that Docker is up and running on your machine (out of scope for this documentation).

## Record a shell session

This is as simple as running

```shell
% asciinema rec recording.json
```

This will launch a sub-shell to be recorded. Once done, exit the sub-shell with `exit`.

## Convert the recording to a gif

```shell
% docker run --rm -v $PWD:/data asciinema/asciicast2gif -t tango -s 2 recording.json recording.gif
```
