# snapshots — prebuilt language-runtime templates

Ready-to-use CubeSandbox templates ("snapshots") for common language runtimes,
all built **on top of `cubesandbox-base`** so they ship `envd` on `:49983` and
work out of the box with the SDK (`sandbox.commands.run(...)`, `files`, etc.).

> Why `cubesandbox-base`? Every sandbox needs the in-guest `envd` agent. A plain
> `python:3.x` / `node` image has no `envd`, so the readiness probe at
> `:49983/health` never comes up and the template build / every SDK command
> fails. See `docs/guide/tutorials/bring-your-own-image.md`.

## Runtimes

| Folder | Template ID | Runtime |
|---|---|---|
| `zig-0.16.0`   | `zig016`  | Zig 0.16.0 |
| `node-latest`  | `node`    | Node.js (current) |
| `bun-latest`   | `bun`     | Bun (latest) |
| `python-3.10`  | `py310`   | CPython 3.10 |
| `python-3.11`  | `py311`   | CPython 3.11 |
| `python-3.12`  | `py312`   | CPython 3.12 |
| `python-3.13`  | `py313`   | CPython 3.13 |
| `python-3.13t` | `py313t`  | CPython 3.13 free-threaded (no-GIL) |
| `python-3.14`  | `py314`   | CPython 3.14 |
| `python-3.14t` | `py314t`  | CPython 3.14 free-threaded (no-GIL) |

## Build

Run on an installed, healthy CubeSandbox control node (needs `docker` +
`cubemastercli`):

```bash
# all runtimes
./snapshots/build-snapshots.sh

# a subset (by template id or folder name)
./snapshots/build-snapshots.sh py313t bun zig016
```

Each runtime is built into a local image `cube-snap/<id>:latest` and registered
as a Cube template via `cubemastercli tpl create-from-image ... --probe 49983`.

Override the base image / writable layer:

```bash
CUBE_SNAPSHOT_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16 \
CUBE_SNAPSHOT_WRITABLE_LAYER=4Gi \
  ./snapshots/build-snapshots.sh
```

## As part of installation

`deploy/one-click/install.sh` will run this automatically when
`CUBE_BUILD_SNAPSHOTS=1` is set and a `snapshots/` folder is present next to the
checkout (source installs). The packaged one-click bundle does not ship this
folder, so set it only for source/dev installs or run the script manually
post-install.

## Use

```python
from cubesandbox import Sandbox
with Sandbox.create(template="py313t") as sb:
    print(sb.commands.run("python3 -VV").stdout)          # free-threaded build
    print(sb.commands.run("python3 -c 'import sys; print(sys._is_gil_enabled())'").stdout)
```
