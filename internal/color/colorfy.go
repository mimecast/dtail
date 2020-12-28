package color

import (
	"fmt"
	"strings"
)

// Add some color to log lines received from remote servers.
func paintRemote(line string) string {
	splitted := strings.Split(line, "|")
	if splitted[2] == "100" {
		splitted[2] = Paint(BgGreen, splitted[2])
	} else {
		splitted[2] = Paint(BgRed, splitted[2])
	}
	info := strings.Join(splitted[0:5], "|")
	log := strings.Join(splitted[5:], "|")

	if strings.HasPrefix(log, "WARN") {
		log = Paint(BgYellow, log)
	} else if strings.HasPrefix(log, "ERROR") {
		log = Paint(BgRed, log)
	} else if strings.HasPrefix(log, "FATAL") {
		log = Attr(Bold, Paint(BgRed, log))
	} else {
		log = Paint(Blue, log)
	}

	return fmt.Sprintf("%s|%s", info, log)
}

// Add some color to stats generated by the client.
func paintClientStats(line string) string {
	splitted := strings.Split(line, "|")
	first := strings.Join(splitted[0:4], "|")
	connected := Paint(BgBlue, splitted[4])
	last := strings.Join(splitted[5:], "|")

	return fmt.Sprintf("%s|%s|%s", first, connected, last)
}

// Colorfy a given line based on the line's content.
func Colorfy(line string) string {
	switch {
	case strings.HasPrefix(line, "REMOTE"):
		return paintRemote(line)
	case strings.HasPrefix(line, "CLIENT") && strings.Contains(line, "|stats|"):
		return paintClientStats(line)
	case strings.Contains(line, "ERROR"):
		return Paint(Magenta, line)
	case strings.Contains(line, "WARN"):
		return Paint(Magenta, line)
	}

	return line
}
