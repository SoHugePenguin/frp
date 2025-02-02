// Copyright 2019 fatedier, fatedier@gmail.com
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package xlog

import (
	"context"
)

type key int

const (
	xLogKey key = 0
)

func NewContext(ctx context.Context, xl *Logger) context.Context {
	return context.WithValue(ctx, xLogKey, xl)
}

func FromContext(ctx context.Context) (xl *Logger, ok bool) {
	xl, ok = ctx.Value(xLogKey).(*Logger)
	return
}

func FromContextSafe(ctx context.Context) *Logger {
	xl, ok := ctx.Value(xLogKey).(*Logger)
	if !ok {
		xl = New()
	}
	return xl
}
