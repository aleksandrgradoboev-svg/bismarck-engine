# Third-party notice: quickjs.wasm

`quickjs.wasm` in this directory is taken unmodified from the npm package
[`quickjs-wasi`](https://github.com/vercel-labs/quickjs-wasi) 3.6.2
(`sha512-FCqGtGOrMgzUiIrMNMA2YnsOxCNwo31dzqXvclXUC6xeT35NJLKXQJsvbeCTjvoFAwZgEAPg8U6+KAPDGXn8Mg==`,
the registry's integrity value, checked on download). It contains two works,
both under the MIT License:

- **quickjs-wasi**, the C interface layer and the build — Copyright (c) 2026
  Vercel, Inc. License text: [`LICENSE.quickjs-wasi`](LICENSE.quickjs-wasi).
- **QuickJS-ng**, the JavaScript engine, at commit
  `6d46d07d04041b40f4f49eaa7fdebe44c314c699` — Copyright (c) 2017-2026 Fabrice
  Bellard, Charlie Gordon, Ben Noordhuis, Saúl Ibarra Corretgé. License text:
  [`LICENSE.quickjs-ng`](LICENSE.quickjs-ng), taken from that commit.

The Go code in this directory is part of this module and under its license.
