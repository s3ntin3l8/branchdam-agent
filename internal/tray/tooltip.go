package tray

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/ingest"
)

// FormatTooltip returns the tooltip text to display on the tray icon
// depending on whether an ingest is currently in flight and what progress
// has been reported.
func FormatTooltip(st Status) string {
	if !st.Busy {
		return "branchDAM agent"
	}
	if st.IngestProgress == nil {
		card := filepath.Base(st.BusyCard)
		if card == "" || card == "." {
			card = st.BusyCard
		}
		if card != "" {
			return fmt.Sprintf("Ingesting %s...", card)
		}
		return "Ingesting..."
	}
	return FormatIngestProgress(st.BusyCard, st.BusySince, st.IngestProgress)
}

// FormatIngestProgress formats a live ingest progress readout into:
// "Ingesting <card>: <filename> — <bytesDone> / <totalBytes> (<pct>%, <speed>, ~<eta>)"
func FormatIngestProgress(cardPath string, busySince time.Time, p *ingest.ProgressEvent) string {
	card := filepath.Base(cardPath)
	if card == "" || card == "." {
		card = cardPath
	}

	fileName := filepath.Base(p.Path)
	if fileName == "" || fileName == "." {
		fileName = p.Path
	}

	bytesDoneStr := formatBytes(p.BytesDone)
	totalBytesStr := formatBytes(p.TotalBytes)

	var pct int
	if p.TotalBytes > 0 {
		pct = int((float64(p.BytesDone) / float64(p.TotalBytes)) * 100)
		if pct > 100 {
			pct = 100
		}
	}

	var speedStr string
	var etaStr string
	var rate float64
	if !busySince.IsZero() {
		elapsed := time.Since(busySince)
		if elapsed > 0 && p.BytesDone > 0 {
			rate = float64(p.BytesDone) / elapsed.Seconds()
		}
	}
	if rate > 0 {
		speedStr = formatSpeed(rate)
		remainingBytes := p.TotalBytes - p.BytesDone
		if remainingBytes > 0 {
			remainingSec := float64(remainingBytes) / rate
			etaStr = formatETA(time.Duration(remainingSec * float64(time.Second)))
		}
	}

	var stats []string
	stats = append(stats, fmt.Sprintf("%d%%", pct))
	if speedStr != "" {
		stats = append(stats, speedStr)
	}
	if etaStr != "" {
		stats = append(stats, etaStr)
	}
	statsJoined := strings.Join(stats, ", ")

	if card != "" {
		return fmt.Sprintf("Ingesting %s: %s \u2014 %s / %s (%s)", card, fileName, bytesDoneStr, totalBytesStr, statsJoined)
	}
	return fmt.Sprintf("Ingesting: %s \u2014 %s / %s (%s)", fileName, bytesDoneStr, totalBytesStr, statsJoined)
}

func formatBytes(b int64) string {
	const (
		kb = 1024
		mb = 1024 * kb
		gb = 1024 * mb
		tb = 1024 * gb
	)
	switch {
	case b >= gb:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(gb))
	case b >= mb:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(mb))
	case b >= kb:
		return fmt.Sprintf("%.1f KB", float64(b)/float64(kb))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func formatSpeed(rate float64) string {
	const (
		kb = 1024
		mb = 1024 * kb
		gb = 1024 * mb
	)
	switch {
	case rate >= float64(gb):
		return fmt.Sprintf("%.1f GB/s", rate/float64(gb))
	case rate >= float64(mb):
		return fmt.Sprintf("%.0f MB/s", rate/float64(mb))
	case rate >= float64(kb):
		return fmt.Sprintf("%.0f KB/s", rate/float64(kb))
	default:
		return fmt.Sprintf("%.0f B/s", rate)
	}
}

func formatETA(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("~%d hr", int(d.Hours()))
	case d >= time.Minute:
		return fmt.Sprintf("~%d min", int(d.Minutes()))
	default:
		return fmt.Sprintf("~%ds", int(d.Seconds()))
	}
}
