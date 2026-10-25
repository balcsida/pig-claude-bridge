# Third-party notices

pig-claude-bridge is MIT licensed (see [LICENSE](LICENSE)). It is a port of, and derives from, the projects below. No source of these projects is vendored; the Go code was written against them, and the license texts are reproduced here as their terms require.

## pi-claude-bridge

- Source: https://github.com/elidickinson/pi-claude-bridge, ported at commit `6aa276a`
- License: MIT, Copyright (c) 2026 Eli Dickinson

```text
MIT License

Copyright (c) 2026 Eli Dickinson

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

## claude-agent-sdk-pi

- Source: https://github.com/prateekmedia/claude-agent-sdk-pi
- License: MIT, Prateek Sunal. pi-claude-bridge was originally based on this project, so it is the origin of the bridge. Its copyright line and text were not available when this notice was written; the MIT terms are those reproduced above for pi-claude-bridge, and the copyright line should be taken from that project's own `LICENSE`.

## cc-session-io

- Source: https://github.com/elidickinson/cc-session-io, version 0.4.0
- License: MIT, Eli Dickinson (the package's `LICENSE` file reads "Copyright (c) 2024" without a name). `internal/ccsession` is a Go port of the subset the bridge uses (session paths, records, chained writes, tool-pairing repair).

```text
MIT License

Copyright (c) 2024

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

## PiG SDK

- Source: https://github.com/MichaelKinsy/PiG, `extensions/sdk` v0.5.3, which this extension imports as a Go module
- License: MIT, Michael Kinsy

```text
MIT License

Copyright Michael Kinsy

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

## golang.org/x/text

`internal/ccsession` imports `golang.org/x/text/unicode/norm` (v0.43.0) for the NFC normalisation of project paths. It is licensed under the BSD 3-Clause license of the Go authors; the license text ships with the module.

## Claude Agent SDK

The protocol client in `internal/ccproto` was written from the Agent SDK's public type definitions and live probes of the installed Claude Code. The SDK itself (`@anthropic-ai/claude-agent-sdk`) is not vendored, bundled or depended on. Claude Code is a separate program, installed and licensed by the user.
