# ElasticClaw

Control plane for provisioning and managing OpenClaw agents from trusted templates across multiple execution providers.

## Overview

ElasticClaw provisions trusted OpenClaw agents from versioned templates, runs them on pluggable providers (like Daytona), and binds each one to scoped, short-lived identity through Creddy.

## Installation

```bash
go install github.com/elasticclaw/elasticclaw@latest
```

## Quick Start

```bash
# Initialize from a template
elasticclaw init --template github.com/acme/support-claw

# Create an instance
elasticclaw create --name support-01 --provider daytona

# Chat with the instance
elasticclaw chat support-01 "what can you help me with?"

# List instances
elasticclaw list

# Destroy when done
elasticclaw destroy support-01
```

## Commands

- `elasticclaw init` - Initialize working directory from a template
- `elasticclaw create` - Create a new instance
- `elasticclaw list` - List instances
- `elasticclaw inspect` - Show instance details
- `elasticclaw chat` - Send messages to an instance
- `elasticclaw destroy` - Destroy an instance
- `elasticclaw template new` - Scaffold a new template
- `elasticclaw template validate` - Validate a template
- `elasticclaw profile` - Manage profiles
- `elasticclaw provider list` - List available providers
- `elasticclaw identity resolve` - Show resolved identity config

## Configuration

Config lives in `~/.elasticclaw/config.yaml`:

```yaml
active_profile: default

catalogs:
  - https://catalog.elasticclaw.dev/images.yaml
```

Profiles live in `~/.elasticclaw/profiles/`:

```yaml
# ~/.elasticclaw/profiles/work.yaml
provider: daytona
state: local
identity: creddy://acme/default
namespace: acme
```

## License

Apache 2.0
