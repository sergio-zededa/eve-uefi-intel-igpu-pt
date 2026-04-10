# EVE-Specific Cloud-Init Extensions (`eve_config`)

EVE supports an optional `eve_config` top-level key in cloud-config user-data.
Settings placed under this key are consumed by EVE **before** the VM is launched
and are then **stripped from the YAML** before it is written to the guest's
cloud-init ISO. This means:

- The guest's cloud-init implementation never sees `eve_config` and will not
  produce warnings or errors for an unknown key.
- The settings take effect regardless of the cloud-init version installed in the
  guest, or even when no cloud-init is installed at all.

## How it works

```
Controller
  │
  │  AppInstanceConfig.UserData (base64-encoded cloud-config)
  ▼
EVE (zedagent → domainmgr)
  │
  ├─ Reads  eve_config.*  → applies settings to DomainStatus
  ├─ Strips eve_config    from the YAML
  │
  └─ Writes sanitised YAML to cloud-init ISO  →  Guest VM
```

The `eve_config` block is re-evaluated on every domain lifecycle event
(create, modify, purge, restart), so changes take effect after the next
VM restart.

## Supported settings

### `disable_vmx` (bool, default: `false`)

Disables Intel VMX (nested virtualisation) for the VM by appending `,-vmx` to
the QEMU `-cpu` argument.

| Property | Value |
|---|---|
| Applies to | KVM/HVM and FML app instances on x86-64 hosts |
| Does **not** apply to | Xen, ARM, containers |
| QEMU effect (HVM) | `-cpu host,-vmx` instead of `-cpu host` |
| QEMU effect (FML) | `-cpu host,hv_time,...,kvm=off,-vmx` instead of `...,kvm=off` |

**Use case:** Running an application inside an EVE-hosted VM that must not be
able to launch nested VMs itself (e.g. security-sensitive workloads, licence
compliance, or guests that mis-detect the VMX flag and malfunction).

## Usage

Set the app instance's `UserData` field to a cloud-config document that
includes an `eve_config` section. The document must start with the
`#cloud-config` header.

### Minimal example — disable VMX only

```yaml
#cloud-config
eve_config:
  disable_vmx: true
```

### Combined example — disable VMX and write a file into the guest

```yaml
#cloud-config
eve_config:
  disable_vmx: true
write_files:
  - path: /etc/myapp/config.toml
    content: |
      nested_virt = false
    permissions: "0644"
```

EVE processes and removes `eve_config`, then passes the remainder to the guest:

```yaml
#cloud-config
write_files:
  - path: /etc/myapp/config.toml
    content: |
      nested_virt = false
    permissions: "0644"
```

## Verifying the setting

On the EVE device, check the domain status log for the line:

```
setEveConfigFromCloudInit: DisableVMX enabled for domain <app-name>
```

Inside a running guest, confirm VMX is hidden:

```sh
grep -c vmx /proc/cpuinfo   # should print 0
```

## Lifecycle behaviour

`eve_config` settings are reset and re-derived from the current `UserData` on
every `configToStatus` call. This covers all domain lifecycle transitions:

| Transition | Behaviour |
|---|---|
| **Create** | Settings derived from `UserData` |
| **Purge** | Reset to defaults, then re-derived from current `UserData` |
| **Modify** (cloud-init changed) | Reset, then re-derived from new `UserData` |
| **Modify** (`disable_vmx` removed) | Flag cleared; VMX re-enabled after restart |
| **Modify** (cloud-init removed entirely) | All `eve_config` settings revert to defaults |
| **Restart** (no cloud-init change) | Same value re-applied from unchanged `UserData` |

## Implementation reference

| File | Change |
|---|---|
| `pkg/pillar/utils/cloudconfig/cloudconfig.go` | `EveConfig` struct; `EveConfig` field on `CloudConfig`; `StripEveConfig()` helper |
| `pkg/pillar/types/domainmgrtypes.go` | `DisableVMX bool` field on `DomainStatus` |
| `pkg/pillar/cmd/domainmgr/domainmgr.go` | `setEveConfigFromCloudInit()` called in `configToStatus()`; `StripEveConfig()` applied before ISO creation |
| `pkg/pillar/hypervisor/kvm.go` | `,-vmx` appended to QEMU `-cpu` arg when `status.DisableVMX && amd64` |

## Extending `eve_config`

To add a new setting:

1. Add a field to `EveConfig` in
   `pkg/pillar/utils/cloudconfig/cloudconfig.go`.
2. Apply the value to `DomainStatus` (or `DomainConfig`) inside
   `setEveConfigFromCloudInit()` in
   `pkg/pillar/cmd/domainmgr/domainmgr.go`.
3. Reset the field to its default at the top of `configToStatus()` alongside
   `status.DisableVMX = false`, so it is always re-evaluated on modify/purge.
4. Consume the field wherever the hypervisor or domain manager needs it
   (e.g. `kvm.go`, `xen.go`).
