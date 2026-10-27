// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

package conntracksync

import "testing"

func TestPartialKernelCommitIsNotSuccess(t *testing.T) {
	// conntrackd itself exits zero for a partial kernel commit. These are the
	// two records emitted by the bundled completion patch after that commit.
	if err := committed([]byte("{\"cache\":\"conntrack\",\"failed\":1}\n{\"cache\":\"expectation\",\"failed\":0}\n")); err == nil {
		t.Fatal("partial commit admitted forwarding")
	}
	if err := committed([]byte("{\"cache\":\"conntrack\",\"failed\":0}\n{\"cache\":\"expectation\",\"failed\":0}\n")); err != nil {
		t.Fatal(err)
	}
}
