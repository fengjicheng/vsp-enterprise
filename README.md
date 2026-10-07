# Vibing Steampunk Enterprise (`vsp`)

## Introduction

`vsp` is an MCP server and CLI for SAP ABAP Development Tools (ADT). It connects MCP-capable assistants (for example Claude Code, Copilot, Codex, and others) to SAP systems so they can read, analyze, and modify ABAP artifacts through controlled tool calls.

This repository is an enterprise-focused fork of the original [Vibing Steampunk](https://github.com/oisee/vibing-steampunk) project by [Alice Vinogradova](https://github.com/oisee). The fork focuses on MCP-first workflows, multi-system operations, and enterprise-ready configuration and safety controls.

### Available Tools

| Tool Group | What it does |
|---|---|
| `System` | System metadata and capability checks |
| `Read` | Read ABAP sources and repository objects |
| `Unified` | Unified object operations via a consolidated interface |
| `GrepSource` | Search patterns inside ABAP source content |
| `FileSource` | File-based source import and export workflows |
| `Analysis` | Call graph and code analysis helpers |
| `Transport` | Transport-related operations and controls |
| `Context` | Context gathering for agent workflows |
| `ATC` | ABAP Test Cockpit checks |
| `ClassInclude` | Class include and method-level handling |
| `CodeIntel` | Definition/reference style code intelligence |
| `CRUD` | Create/update/delete style repository actions |
| `Dev` | Developer operations (syntax checks, activation, tests) |
| `Dump` | Runtime dump diagnostics |
| `File` | File-centric utility operations |
| `Git` | abapGit-related integrations |
| `Grep` | Object-level grep and repository search |
| `Report` | Report execution and report metadata |
| `ServiceBinding` | RAP service binding operations |
| `SQLTrace` | SQL trace access and inspection |
| `Trace` | Trace listing and trace retrieval |
| `Workflow` | Workflow-oriented tool operations |
| `DebuggerLegacy` | Legacy debugger-compatible operations |

## Setup & Requirements

### Requirements

- Go `1.24` (from `go.mod`)
- Network access to SAP ADT endpoint(s)
- Optional for RFC sidecar usage: Java + Maven in `sidecar/jco-proxy`

### Install from Release (recommended)

```bash
curl -LO https://github.com/BurnerPat/vsp-enterprise/releases/latest/download/vsp-linux-amd64
chmod +x vsp-linux-amd64
mv vsp-linux-amd64 vsp
./vsp --version
```

### Build from Source

```bash
git clone https://github.com/BurnerPat/vsp-enterprise.git
cd vsp-enterprise
make build
./build/vsp --version
```

## Usage

### Start MCP server with direct connection flags

```bash
./build/vsp --url https://sap-host:44300 --user DEVELOPER --password secret --client 001
```

### Start MCP server from named profile

```bash
./build/vsp --system dev
```

### Start with multiple configured systems

```bash
./build/vsp --multi-system
```

### Authentication methods

`vsp` supports multiple authentication flows, depending on connection mode and enterprise setup:

- **Username/password (HTTP ADT)** for standard Basic auth.
- **Cookie auth (HTTP ADT)** using either `--cookie-file` (Netscape format) or `--cookie-string`.
- **Browser-based SSO (HTTP ADT)** via `--browser-auth` for Kerberos/SAML/Keycloak flows. By default VSP uses the system browser and an ADT reentrance-ticket callback, so the browser's normal profile, active sessions, and password manager remain available.
- **SNC/SSO (RFC mode)** using `--connection-mode rfc --snc --sysid <SID>` with SAP UI Landscape/JCo settings.

Examples:

```bash
# 1) Username/password (HTTP ADT)
./build/vsp --url https://sap-host:44300 --user DEVELOPER --password secret --client 001

# 2) Cookie file (HTTP ADT)
./build/vsp --url https://sap-host:44300 --cookie-file ./cookies.txt

# 3) Browser SSO in the normal system browser (HTTP ADT)
./build/vsp --url https://sap-host:44300 --browser-auth

# 4) SNC/SSO (RFC mode)
./build/vsp --connection-mode rfc --snc --sysid QAS --client 200
```

#### Browser SSO callback flow

With `--browser-auth`, VSP starts a temporary HTTP listener on the loopback interface and opens the SAP ADT reentrance-ticket endpoint in the system browser. After SSO completes, SAP redirects to the local listener. VSP exchanges the short-lived ticket for a stateful ADT session and then shuts down the listener.

For a named system in `.vsp.json`:

```json
{
  "systems": {
    "cloud-dev": {
      "url": "https://example.abap-web.eu10.hana.ondemand.com",
      "browser_auth": true,
      "browser_auth_timeout": "120s"
    }
  }
}
```

Set `--browser-exec` or `browser_exec` to use the automated Chromium fallback instead. A custom `--browser-auth-url` also requires that fallback because arbitrary login targets cannot redirect the reentrance ticket to VSP. The automated browser uses a dedicated persistent profile; `VSP_BROWSER_DATA_DIR` overrides its location.

### Sharing one backend across MCP clients

By default every MCP client session (each Claude Code chat, Copilot, …) starts its own VSP process with its own SAP session, JCo sidecar and SSO login. Add `--shared` to the server arguments to let all sessions with the same configuration share one background daemon instead:

```json
"args": ["--multi-system", "--config", "/path/to/config.json", "--shared"]
```

The MCP client configuration stays stdio-based. The first session starts the daemon; further sessions attach to it over a per-user Unix domain socket (macOS, Linux and Windows 10 1803+). The daemon stops after `--shared-idle-timeout` (default `10m`) without clients. A different binary or configuration gets its own daemon. If no daemon can be started, VSP serves the session in-process as before.

```bash
./build/vsp daemon status   # list daemons, connected clients, systems
./build/vsp daemon stop     # stop all daemons; open sessions respawn one on their next request
```

Daemon logs are written to `~/Library/Logs/vsp/` (macOS) or `<user cache dir>/vsp/logs/` (Linux: `~/.cache`, Windows: `%LOCALAPPDATA%`). All sessions share the SAP session, so object locks and the debugger are shared too. On Windows, run `vsp daemon stop` before replacing `vsp.exe`.

### Useful CLI utilities

```bash
./build/vsp systems
./build/vsp config init
./build/vsp config show
./build/vsp config tools list
./build/vsp jco status
./build/vsp jco setup --system dev
```

## Configuration

`vsp` uses standard precedence:

1. CLI flags
2. Environment variables
3. `.env`
4. Config defaults

### Config files

- `.env` for default connection values (`SAP_URL`, `SAP_USER`, `SAP_PASSWORD`, `SAP_CLIENT`, ...)
- `.vsp.json` for named systems, roles, and permissions

`vsp` searches for `.vsp.json` in this order:

1. `.vsp.json`
2. `.vsp/systems.json`
3. `~/.vsp.json`
4. `~/.vsp/systems.json`

### Minimal `.vsp.json`

```json
{
  "default": "dev",
  "systems": {
    "dev": {
      "url": "https://sap-dev.example.com:44300",
      "user": "DEVELOPER",
      "client": "001"
    },
    "prod": {
      "url": "https://sap-prod.example.com:44300",
      "user": "READONLY",
      "client": "100",
      "read_only": true
    }
  }
}
```

### Role-based permissions

Permissions are managed through **roles** — named sets of tool and object access rules defined at the top level and assigned to systems. Each system lists the roles it uses, and `vsp` merges them at startup to compute the effective permissions.

#### Key concepts

- **Roles** are defined under `roles` in `.vsp.json`. Each role lists tool patterns and optional object-level restrictions.
- **Systems** reference roles by name via the `roles` array. A system can combine multiple roles.
- **Nested roles** — a role can include other roles via `nested_roles` for composition.
- **Built-in `default` role** — when a system has no `roles` array, it receives the built-in `default` role which grants unrestricted access to all tools. You can override the `default` role by defining your own.
- **Deny wins** — if any role explicitly disables a tool (`"enabled": false`), the tool is blocked regardless of other roles.
- **Object restrictions** use union semantics: `allowed_objects` and `allowed_packages` from all roles are merged; `blocked_objects` from any role always blocks.

#### Tool patterns

Tool patterns in roles support `*` as a wildcard:

| Pattern | Matches |
|---|---|
| `*` | All tools |
| `Get*` | All tools starting with `Get` |
| `*Source*` | All tools containing `Source` |
| `RunQuery` | Exact match |

#### Tool permission fields

Each tool entry in a role can specify:

| Field | Type | Description |
|---|---|---|
| `enabled` | `bool` | Explicitly allow (`true`) or deny (`false`). Omit to inherit. |
| `allowed_packages` | `string[]` | Only allow operations on objects in these packages (glob patterns). |
| `allowed_objects` | `string[]` | Only allow operations on these objects (glob patterns). |
| `blocked_objects` | `string[]` | Block operations on these objects (glob patterns, deny wins). |

#### Example `.vsp.json` with roles

```json
{
  "default": "dev",
  "roles": {
    "reader": {
      "description": "Read-only access to ABAP sources",
      "tools": {
        "Get*": {},
        "Search*": {},
        "List*": {}
      }
    },
    "developer": {
      "description": "Full development access with package restrictions",
      "nested_roles": ["reader"],
      "tools": {
        "Write*": { "allowed_packages": ["Z*", "Y*"] },
        "Create*": { "allowed_packages": ["Z*", "Y*"] },
        "Delete*": { "enabled": false },
        "Activate*": {},
        "Run*": {}
      }
    },
    "prod_auditor": {
      "description": "Production read-only with sensitive table protection",
      "tools": {
        "Get*": {},
        "Search*": {},
        "DataPreview": {
          "blocked_objects": ["USR*", "T000", "RFCDES"]
        }
      }
    }
  },
  "systems": {
    "dev": {
      "url": "https://sap-dev.example.com:44300",
      "user": "DEVELOPER",
      "client": "001",
      "roles": ["developer"]
    },
    "prod": {
      "url": "https://sap-prod.example.com:44300",
      "user": "READONLY",
      "client": "100",
      "roles": ["prod_auditor"]
    }
  }
}
```

#### Discovery

Use the `ListAvailableTools` meta-tool (always available, bypasses permissions) to inspect what each system can access. The response includes enabled tools and any object-level restrictions per system.

For per-system passwords with named systems, use environment variables such as `VSP_DEV_PASSWORD`.

When no username is provided via `--user`, `SAP_USER`, or a system's `user` field, `vsp` automatically defaults to the current OS login name (uppercased). Override it at any level to use a different SAP account.

## Contribution

Contributions are welcome.

Preferred contribution flow:

```bash
git checkout -b feature/my-change
make fmt
make test
make build
```

Then open a pull request with:

- clear problem statement
- implementation summary
- test notes

For project background and architecture context, see `docs/architecture.md` and `FORK.md`.

## License

MIT License. See `LICENSE`.

This repository is a fork of the original [Vibing Steampunk](https://github.com/oisee/vibing-steampunk) project created by [Alice Vinogradova](https://github.com/oisee). Credit for the original work and contributors remains with the upstream project.
