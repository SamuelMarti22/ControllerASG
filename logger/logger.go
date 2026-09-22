package logger

import (
	"encoding/json"
	"io"
	"sync"

	"controllerasg/model"
)

// JSONLogger escribe un registro de decisión por línea (JSON Lines).
type JSONLogger struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func New(w io.Writer) *JSONLogger {
	return &JSONLogger{enc: json.NewEncoder(w)}
}

func (l *JSONLogger) Log(rec model.DecisionRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.enc.Encode(rec)
}
