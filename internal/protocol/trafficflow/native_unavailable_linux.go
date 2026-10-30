// SPDX-License-Identifier: BSD-3-Clause
//go:build !cgo || !libndpi

package trafficflow

import (
	"errors"
	"github.com/imksoo/routerd/pkg/dpi"
	"sync/atomic"
	"time"
)

type nativeClassifier struct{}

func newNativeClassifier(*atomic.Uint64) (*nativeClassifier, error) {
	return nil, errors.New("application_layer requires routerd-modules built with libndpi")
}
func (*nativeClassifier) close() {}
func (*nativeClassifier) classify(_ capturedPacket, result dpi.ClassifyResult, _ time.Time) (dpi.ClassifyResult, error) {
	return result, nil
}
