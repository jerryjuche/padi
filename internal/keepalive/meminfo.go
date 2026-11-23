package keepalive

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// MemInfo is the part of /proc/meminfo the keeper needs.
type MemInfo struct {
	TotalKB     int64
	AvailableKB int64
}

// MemReader is the consumer-defined source of memory readings.
type MemReader interface {
	Read() (MemInfo, error)
}

// ProcMemInfo reads the host's /proc/meminfo. An empty Path means /proc/meminfo.
type ProcMemInfo struct {
	Path string
}

// Read implements MemReader.
func (p ProcMemInfo) Read() (MemInfo, error) {
	path := p.Path
	if path == "" {
		path = "/proc/meminfo"
	}
	return readMemInfo(path)
}

// FileMemInfo reads a meminfo-formatted file; useful for tests and fixtures.
type FileMemInfo struct {
	Path string
}

// Read implements MemReader.
func (f FileMemInfo) Read() (MemInfo, error) {
	return readMemInfo(f.Path)
}

func readMemInfo(path string) (MemInfo, error) {
	file, err := os.Open(path)
	if err != nil {
		return MemInfo{}, fmt.Errorf("open meminfo: %w", err)
	}
	defer file.Close()

	var info MemInfo
	var sawTotal, sawAvailable bool
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		var dst *int64
		switch fields[0] {
		case "MemTotal:":
			dst, sawTotal = &info.TotalKB, true
		case "MemAvailable:":
			dst, sawAvailable = &info.AvailableKB, true
		}
		if dst == nil {
			continue
		}
		n, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return MemInfo{}, fmt.Errorf("parse %s: %w", fields[0], err)
		}
		*dst = n
	}
	if err := scanner.Err(); err != nil {
		return MemInfo{}, fmt.Errorf("scan meminfo: %w", err)
	}
	if !sawTotal || !sawAvailable {
		return MemInfo{}, fmt.Errorf("meminfo %s: missing MemTotal or MemAvailable", path)
	}
	return info, nil
}
