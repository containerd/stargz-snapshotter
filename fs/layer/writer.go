package layer

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Buffered writer that writes to a file in a buffered manner, it need to be a singleton
type BufferedWriter struct {
	buf         bytes.Buffer
	ch          chan *Message
	mu          sync.RWMutex
	messages    []*Message
	outputFile  *os.File
	bufferSize  int
	flushTicker *time.Ticker
	done        chan bool
}

func NewBufferedWriter(filename string, bufferSize int, flushInterval time.Duration, messageBufferSize int) (*BufferedWriter, error) {
	// Create root dir
	rootDir := filepath.Dir(filename)
	os.MkdirAll(rootDir, 0755)

	file, err := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %v", err)
	}

	writer := &BufferedWriter{
		mu:          sync.RWMutex{},
		ch:          make(chan *Message, messageBufferSize),
		outputFile:  file,
		bufferSize:  bufferSize,
		flushTicker: time.NewTicker(flushInterval),
		done:        make(chan bool),
	}
	// Run the writer in a goroutine
	go writer.Process()
	return writer, nil
}

func (w *BufferedWriter) Write(info *RecorderInfo, path string) {
	w.ch <- &Message{info: info, path: path}
}

func (w *BufferedWriter) Process() {
	for {
		select {
		case <-w.flushTicker.C:
			if err := w.Flush(); err != nil {
				log.Printf("failed to flush buffer: %v", err)
			}
		case <-w.done:
			if err := w.Flush(); err != nil {
				log.Printf("failed to flush buffer: %v", err)
			}
			w.outputFile.Close()
		case msg := <-w.ch:
			data, err := DataToRawLine(msg.info, msg.path)
			if err != nil {
				log.Printf("failed to convert message to raw line: %v", err)
			}

			// Add newline to separate entries
			data = append(data, '\n')

			// If adding this message would exceed buffer size, flush first
			if w.buf.Len()+len(data) > w.bufferSize {
				if err := w.Flush(); err != nil {
					log.Printf("failed to flush buffer: %v", err)
				}
			}

			w.buf.Write(data)
			w.messages = append(w.messages, msg)
		}
	}

}

func (w *BufferedWriter) Flush() error {
	if w.buf.Len() == 0 {
		return nil
	}

	if _, err := w.outputFile.Write(w.buf.Bytes()); err != nil {
		return fmt.Errorf("failed to write to file: %v", err)
	}

	w.buf.Reset()
	w.messages = w.messages[:0]
	return nil
}

func (w *BufferedWriter) Close() error {
	w.done <- true
	return nil
}
