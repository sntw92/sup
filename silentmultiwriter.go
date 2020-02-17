package sup

import "io"

type silentMultiWriter struct {
	writers []io.Writer
}

func (t *silentMultiWriter) Write(p []byte) (n int, err error) {
	for _, w := range t.writers {
		n, _ = w.Write(p)
	}
	return len(p), nil
}

func (t *silentMultiWriter) WriteString(s string) (n int, err error) {
	var p []byte // lazily initialized if/when needed
	for _, w := range t.writers {
		if sw, ok := w.(io.StringWriter); ok {
			n, _ = sw.WriteString(s)
		} else {
			if p == nil {
				p = []byte(s)
			}
			n, _ = w.Write(p)
		}
	}
	return len(s), nil
}

// SilentMultiWriter creates a writer that duplicates its writes to all the
// provided writers, similar to the Unix tee(1) command.
//
// Each write is written to each listed writer, one at a time.
// If a listed writer returns an error, it is silently ignored
func SilentMultiWriter(writers ...io.Writer) io.Writer {
	allWriters := make([]io.Writer, 0, len(writers))
	for _, w := range writers {
		if mw, ok := w.(*silentMultiWriter); ok {
			allWriters = append(allWriters, mw.writers...)
		} else {
			allWriters = append(allWriters, w)
		}
	}
	return &silentMultiWriter{allWriters}
}
