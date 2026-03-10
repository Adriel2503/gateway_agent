package domain

import "errors"

// ErrEmptyReply indica que el agente respondio HTTP 200 pero con reply vacio.
// Se define en domain para que proxy y handler puedan usarlo sin acoplarse entre si.
var ErrEmptyReply = errors.New("agent returned empty reply")
