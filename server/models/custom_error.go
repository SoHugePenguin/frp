package models

import "fmt"

// PenguinErr 与java不同，go的接口与其实现是隐性的
type PenguinErr struct {
	Reason string
	Code   int
}

// 实现接口
func (e *PenguinErr) Error() string {
	return fmt.Sprintf("Penguin error: %s (Code: %d)", e.Reason, e.Code)
}

// InitError 自定义错误
func InitError(reason string, code int) error {
	return &PenguinErr{
		Reason: reason,
		Code:   code,
	}
}
