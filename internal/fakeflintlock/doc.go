/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package fakeflintlock is the fake flintlockd
// (docs/requirements/08-test-doubles.md#fake-flintlockd): a flintlock
// MicroVM and MicroVMExec server on the generated stubs of flintlock's api
// module, whose MicroVMs exist only in memory.
//
// A Server is reached in two ways. Conn dials it in process over an
// in-memory listener, which is what the fake battery and most tests use;
// Serve adds a TCP listener, with TLS and client-certificate verification
// when the configuration asks for them, for code that dials an address.
// Both go through the same handlers and the same fault injection.
//
// ExecCommand runs no process. The first message is checked as flintlockd
// checks it, and the command is then handed to the Server's ExecFunc, which
// a test sets with SetExec: it sees the ExecStart, reads the client's
// standard input and writes standard output and standard error, and its
// result becomes the exit status, an error followed by the exit status, or
// a stream cut before the exit status. Script builds an ExecFunc from a
// canned Reply. Without one, every command succeeds silently. Execs records
// every command the Server accepted, in order.
//
// One Server has one lifetime. It starts at New and ends at Close, or when
// the context given to Serve is cancelled. Closing cancels every running
// exec and stops every pending boot, and every later call fails with
// UNAVAILABLE.
//
// Behaviour is modelled on flintlockd where a client can observe it: the
// same status codes, the same first-message rule on ExecCommand and the
// same error-then-exit_code framing. The fake differs in these ways:
//
//   - A MicroVM is a record in memory. Its vsock_path names no socket, and
//     each interface of its spec is reported with a made-up host device.
//   - DeleteMicroVM is synchronous: the MicroVM is gone when it returns,
//     where flintlockd moves it through DELETING first.
//   - A spec with no id takes its uid as its id, and one with no namespace
//     goes into "default"; flintlockd refuses both.
//   - ExecCommand does not require allow_guest_agent on the spec, and
//     timeout_seconds, cwd, env and user are passed to the ExecFunc in the
//     ExecStart rather than enforced.
//   - ServerInfo reports the configured version and exec flag, and the SSH
//     proxy as disabled; the fake serves no MicroVMSSHProxy.
//   - There is no basic auth. Hosts are reached over mutual TLS
//     (docs/adr/0002-battery-reaches-flintlockd-over-mtls.md).
package fakeflintlock
