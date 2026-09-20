package main

import (
	"bufio"
	"errors"
	"io"
	"syscall"
)

const (
	kmsgPath = "/dev/kmsg"

	// /dev/kmsg returns EINVAL without consuming records larger than this buffer.
	kmsgRecordBufferBytes = 8192
)

// scanKmsg hands each /dev/kmsg record to handle until the read fails. EPIPE
// means records were overwritten while reading; the fd continues at the next
// available record.
//
// Buffering is safe only because every /dev/kmsg read returns one whole
// newline-terminated record: ReadString drains the buffer each time, so the
// next fill always offers the full kmsgRecordBufferBytes. A partial record
// would shrink that read and turn a normal-sized record into EINVAL.
func scanKmsg(r io.Reader, handle func(record string)) error {
	reader := bufio.NewReaderSize(r, kmsgRecordBufferBytes)
	for {
		record, err := reader.ReadString('\n')
		if record != "" {
			handle(record)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if errors.Is(err, syscall.EPIPE) {
				continue
			}
			return err
		}
	}
}
