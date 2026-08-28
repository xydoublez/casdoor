---
name: sync-ad-to-casdoor
description: Set up and execute AD-to-Casdoor synchronization via LDAP integration, including user sync via API and org tree sync via auto-sync or direct Python ops script
---

# Sync AD to Casdoor

## Overview
Synchronize Active Directory users and organization tree (OUs + security groups) to Casdoor.

## Two Approaches

### Approach A: Pure Casdoor API (existing `scripts/sync_ad_to_casdoor.py`)
- **User sync**: Done immediately via Casdoor API (`/api/sync-ldap-users`)
- **Org tree sync (groups/OUs)**: Only works through Casdoor's auto-sync timer — there is NO independent API endpoint for group sync. Set `autoSync` (minutes) and `enableGroups=true` on the LDAP config to enable. First org tree sync executes after `autoSync` minutes.
- **Limitation**: Cannot trigger group sync on demand; depends on Casdoor's AD group search which can be unstable for large trees.

### Approach B: Python Ops Script (new `scripts/sync_ad_tree.py`) — **Recommended**
- Script connects directly to AD via `ldap3`, reads the full OU tree + users + memberOf.
- Creates/updates Casdoor groups with correct hierarchy immediately (no autoSync wait).
- Syncs users with `ldap` (objectGUID) binding and group membership.
- Falls back to Casdoor LDAP mode if AD credentials are not provided.

## Prerequisites
- Casdoor instance running with admin access
- AD server reachable from the machine running the script
- For Approach A: a Casdoor Application under the `built-in` organization — use its `clientId`/`clientSecret` for API authentication
- For Approach B: admin credentials (ADMIN / PASSWORD) for session-based auth

## Scripts

### `scripts/sync_ad_to_casdoor.py` (Approach A)
Uses Casdoor Application `clientId`/`clientSecret` as API query parameters.

```bash
pip install requests
# Edit CONFIG block at top of script
python3 scripts/sync_ad_to_casdoor.py
```

### `scripts/sync_ad_tree.py` (Approach B) — **Recommended**
Uses admin session authentication. Supports both AD direct-connect and Casdoor LDAP fallback modes.

```bash
pip install requests ldap3
# Set environment variables (see Configuration below)
python3 scripts/sync_ad_tree.py
```

## Configuration Parameters (Approach B)

| Parameter | Env Var | Description |
|-----------|---------|-------------|
| `CASDOOR_URL` | `CASDOOR_URL` | Casdoor base URL (default: `https://portal.msuncloud.cn`) |
| `CASDOOR_ORG` | `CASDOOR_ORG` | Target organization name (default: `zymsunsoft`) |
| `CASDOOR_ADMIN` | `CASDOOR_ADMIN` | Admin account in `org/name` format (default: `built-in/admin`) |
| `CASDOOR_ADMIN_PASSWORD` | `CASDOOR_ADMIN_PASSWORD` | Admin password (required) |
| `CASDOOR_LOGIN_APP` | `CASDOOR_LOGIN_APP` | Login application name (default: `app-built-in`) |
| `AD_HOST` | `AD_HOST` | AD server IP/domain (empty = Casdoor LDAP mode) |
| `AD_PORT` | `AD_PORT` | LDAP port (default: 389) |
| `AD_USE_SSL` | `AD_USE_SSL` | Enable LDAPS (default: false) |
| `AD_BIND_DN` | `AD_BIND_DN` | Bind DN (e.g., `CN=svc,OU=...,DC=...`) |
| `AD_BIND_PASSWORD` | `AD_BIND_PASSWORD` | Bind password |
| `AD_BASE_DN` | `AD_BASE_DN` | Search base DN |
| `AD_USER_FILTER` | `AD_USER_FILTER` | User filter (default: `(&(objectClass=user)(objectCategory=person))`) |
| `CASDOOR_AUTOSYNC_MIN` | `CASDOOR_AUTOSYNC_MIN` | Post-sync autoSync interval in minutes (default: 5, 0=disabled) |
| `CASDOOR_DEFAULT_PASSWORD` | `CASDOOR_DEFAULT_PASSWORD` | Placeholder password for AD users (default: `AdSync@2026`) |

## Script Execution Steps (Approach B)

### AD Direct-Connect Mode (when AD_HOST is set):
1. **Authenticate** — Login as admin via `/api/login`, establish session
2. **Read AD** — Connect to AD via ldap3, read all OUs and users with memberOf
3. **Build OU tree** — Derive parent-child hierarchy from OU DNs
4. **Sync groups** — Create/update Casdoor groups in topological order (parents first)
5. **Sync users** — For each AD user: add new or update existing with groups + ldap binding
6. **Verify** — Check no self-references, no cycles, ≥80% users have groups

### Casdoor LDAP Fallback Mode (when AD_HOST is empty):
1. **Authenticate** — Login as admin
2. **Fetch LDAP users** — Call `/api/get-ldap-users` for each configured LDAP
3. **Derive groups** — Extract OU hierarchy from memberOf DN chains
4. **Build missing groups** — Create groups not yet in Casdoor
5. **Sync users** — Call `/api/sync-ldap-users` (server attaches users to memberOf groups)
6. **Fix hierarchy** — Correct parent-child relationships using memberOf-derived tree

## Authentication

### Approach A: Application credentials
Script uses Casdoor Application `clientId`/`clientSecret` as API query parameters. The application's identity `app/<app_name>` has full API access. The Application must belong to the `built-in` organization.

### Approach B: Admin session
Script logs in via `POST /api/login` with:
```json
{
  "type": "login",
  "username": "admin",
  "password": "<PASSWORD>",
  "application": "app-built-in",
  "organization": "built-in"
}
```
Session cookie is maintained via `requests.Session()` for all subsequent API calls.

## AD Org Tree Sync Mechanism (for reference)
- LDAP code: `object/ldap_conn.go`, `object/ldap_autosync.go`
- Auto-detect AD via `isMicrosoftAD()` (checks `isGlobalCatalogReady=TRUE`)
- Groups sync: queries `(objectClass=group)` and `(objectClass=organizationalUnit)`
- Hierarchy preserved via `ParentDn` → `getParentDn()` → `ParentId`/`IsTopGroup` fields
- User-group mapping via AD `memberOf` attribute
- Sync order: groups first, then users
- **Known issue**: `cn=X inside ou=X` produces self-referencing `parentId` — Approach B script fixes this in post-processing

## Verification
- Check script output for group/user sync counts
- Verify no self-references or cycles in group tree
- Check ≥80% of users have group assignments
- Verify top-level group matches AD root OU (e.g., "众阳健康集团")
- Check Casdoor logs for any LDAP sync errors

## How to Run

```bash
# Approach A (Application credentials)
pip install requests
# Edit CONFIG in scripts/sync_ad_to_casdoor.py
python3 scripts/sync_ad_to_casdoor.py

# Approach B (Admin session, recommended)
pip install requests ldap3
export CASDOOR_ADMIN_PASSWORD="your_password"
export AD_HOST="192.168.1.100"
export AD_BIND_DN="CN=svc_ldap,OU=ServiceAccounts,DC=example,DC=com"
export AD_BIND_PASSWORD="your_ad_password"
export AD_BASE_DN="DC=example,DC=com"
python3 scripts/sync_ad_tree.py
```

## Known Frontend Issue
If adding a syncer via Casdoor web UI returns 404, see fix in `web/src/SyncerListPage.js:53` — navigation path must be `/syncers/${org}/${name}` (two segments) to match the route in `ManagementPage.js:592`.
