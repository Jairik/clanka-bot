// Command latency-say synthesizes one phrase through the real TTS service and
// writes raw little-endian PCM to a file. It is a dev helper for the #27
// latency measurement, not part of the assistant.
//
//	latency-say -addr 127.0.0.1:20200 -text "okay nabu" -out wake.pcm
//
// The raw PCM can be played into a null sink with pacat; see
// docs/latency-dev-box.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/wyoming"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:20201", "wyoming TTS address")
	text := flag.String("text", "okay nabu", "text to synthesize")
	voice := flag.String("voice", "alba", "voice name")
	out := flag.String("out", "", "PCM output path")
	flag.Parse()
	if *out == "" {
		fmt.Fprintln(os.Stderr, "latency-say: -out is required")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := wyoming.NewClient(*addr)
	stream, err := client.Synthesize(ctx, *text, wyoming.Voice{Name: *voice})
	if err != nil {
		fmt.Fprintln(os.Stderr, "latency-say:", err)
		os.Exit(1)
	}
	defer stream.Close()

	var pcm []byte
	for {
		chunk, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "latency-say:", err)
			os.Exit(1)
		}
		pcm = append(pcm, chunk...)
	}
	if err := os.WriteFile(*out, pcm, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "latency-say:", err)
		os.Exit(1)
	}
	fmt.Printf("%s %dHz %dch %dbit %d bytes\n", *out, stream.Format.Rate, stream.Format.Channels, stream.Format.Width*8, len(pcm))
}
