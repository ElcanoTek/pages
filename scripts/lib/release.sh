# shellcheck shell=bash
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
# scripts/lib/release.sh — the shell half of pages's signed release artifacts.
# Sourced by scripts/bootstrap.sh and scripts/update.sh.
#
# Its job is the FIRST verified binary on a box (bootstrap, or a box upgrading
# from an older install before `pages update` gained native signed-release
# verification). Everything after that runs through `pages update`
# (internal/ops/update), which implements the same checks in Go with tests.
# Both pin the same trust — the identity and issuer below, and the cosign
# digests in cosign.lock.
#
# Checks, in order, before the binary is used (it is not even chmod +x until
# they pass): the cosign used is the pinned build (sha256), SHA256SUMS carries
# a signature by pages's release workflow on main, the binary and release.json
# match the signed sums, release.json names the requested tag and commit, and
# the binary itself carries a valid signature. There is no unverified fallback.
#
# Needs: curl, sha256sum, awk, and git (tag resolution). Not Go, not make.

PAGES_RELEASE_REPO="ElcanoTek/pages"
PAGES_RELEASE_IDENTITY="https://github.com/ElcanoTek/pages/.github/workflows/release.yml@refs/heads/main"
PAGES_RELEASE_ISSUER="https://token.actions.githubusercontent.com"

# Download ceilings, the same as internal/ops/update (maxMetaBytes,
# maxBinaryBytes): a hostile or broken server cannot fill the disk.
PAGES_RELEASE_MAX_META=$((4 << 20))
PAGES_RELEASE_MAX_BINARY=$((512 << 20))

# pages_release_get URL OUT MAX — download URL to OUT (0600) under a size
# ceiling; a failed or oversized transfer leaves no partial file behind.
pages_release_get() {
  local url="$1" out="$2" max="$3"
  rm -f "$out"
  if ! curl -fsSL --retry 3 --max-filesize "$max" -o "$out" "$url"; then
    rm -f "$out"
    return 1
  fi
  if [[ "$(stat -c %s "$out" 2>/dev/null || echo 0)" -gt "$max" ]]; then
    rm -f "$out"
    echo "${url##*/} is larger than ${max} bytes — refusing it" >&2
    return 1
  fi
  chmod 0600 "$out"
}

# pages_release_arch — this box's release architecture (amd64|arm64), or fail.
pages_release_arch() {
  case "$(uname -m)" in
    x86_64|amd64)  printf 'amd64' ;;
    aarch64|arm64) printf 'arm64' ;;
    *) echo "no pages release is published for $(uname -m) (amd64 and arm64 are)" >&2; return 1 ;;
  esac
}

pages_release_base_url() {
  printf '%s' "${PAGES_RELEASE_BASE_URL:-https://github.com/${PAGES_RELEASE_REPO}/releases/download}"
}

# pages_release_latest_tag — the newest PUBLISHED release (its assets are
# uploaded before the release object is published, so this never names a
# release that is still building).
pages_release_latest_tag() {
  local json tag
  json="$(curl -fsSL --retry 3 --max-filesize "$PAGES_RELEASE_MAX_META" -H 'Accept: application/vnd.github+json' \
    "${PAGES_RELEASE_LATEST_URL:-https://api.github.com/repos/${PAGES_RELEASE_REPO}/releases/latest}")" || return 1
  tag="$(printf '%s' "$json" | grep -o '"tag_name"[[:space:]]*:[[:space:]]*"[^"]*"' | head -n1 | sed 's/.*"\([^"]*\)"$/\1/')"
  [[ "$tag" =~ ^v[0-9]{4}\.[0-9]{2}\.[0-9]{2}\.[1-9][0-9]*$ ]] || { echo "unexpected latest release tag: ${tag:-none}" >&2; return 1; }
  printf '%s' "$tag"
}

# pages_release_cosign REPO_ROOT CACHE_DIR — print the path of the pinned
# cosign, downloading it when the cached copy is absent or not the pinned
# bytes. The digest in cosign.lock is checked on every call.
pages_release_cosign() {
  local root="$1" cache="$2" lock arch ver want path tmp
  lock="$root/cosign.lock"
  [[ -r "$lock" ]] || { echo "missing $lock" >&2; return 1; }
  arch="$(pages_release_arch)" || return 1
  ver="$(awk '$1=="version"{print $2}' "$lock")"
  want="$(awk -v k="linux/$arch" '$1==k{print $2}' "$lock")"
  [[ -n "$ver" && ${#want} -eq 64 ]] || { echo "cosign.lock has no pin for linux/$arch" >&2; return 1; }
  install -d -m 0700 "$cache" && chmod 0700 "$cache" || return 1
  pages_release_private_dir "$cache" || return 1
  path="$cache/cosign-${ver}-linux-${arch}"
  if [[ -n "${PAGES_COSIGN_BIN:-}" ]]; then
    tmp="${path}.copy"
    cp -f --no-preserve=ownership "$PAGES_COSIGN_BIN" "$tmp" || return 1
    if [[ "$(sha256sum "$tmp" | awk '{print $1}')" != "$want" ]]; then
      rm -f "$tmp"
      echo "PAGES_COSIGN_BIN does not match the digest pinned in cosign.lock — refusing to run it" >&2
      return 1
    fi
    chmod 0755 "$tmp" && mv -f "$tmp" "$path" && printf '%s' "$path"
    return 0
  fi
  if [[ -f "$path" ]] && [[ "$(sha256sum "$path" | awk '{print $1}')" == "$want" ]]; then
    printf '%s' "$path"; return 0
  fi
  tmp="${path}.download"
  pages_release_get "${PAGES_COSIGN_URL:-https://github.com/sigstore/cosign/releases/download/${ver}}/cosign-linux-${arch}" "$tmp" "$PAGES_RELEASE_MAX_BINARY" || return 1
  if [[ "$(sha256sum "$tmp" | awk '{print $1}')" != "$want" ]]; then
    rm -f "$tmp"
    echo "downloaded cosign ${ver} does not match the digest pinned in cosign.lock — refusing to run it" >&2
    return 1
  fi
  chmod 0755 "$tmp" && mv -f "$tmp" "$path" && printf '%s' "$path"
}

# pages_release_verify_blob COSIGN FILE — FILE's .sigstore.json bundle must be
# a signature by the release identity.
pages_release_verify_blob() {
  local cosign="$1" file="$2" extra=()
  [[ -f "${file}.sigstore.json" ]] || { echo "no signature for ${file##*/} — refusing an unsigned release" >&2; return 1; }
  [[ -n "${PAGES_SIGSTORE_TRUSTED_ROOT:-}" ]] && extra=(--trusted-root "$PAGES_SIGSTORE_TRUSTED_ROOT")
  "$cosign" verify-blob --bundle "${file}.sigstore.json" \
    --certificate-identity "$PAGES_RELEASE_IDENTITY" \
    --certificate-oidc-issuer "$PAGES_RELEASE_ISSUER" "${extra[@]}" "$file" >/dev/null 2>&1 \
    || { echo "signature verification FAILED for ${file##*/} (signer must be ${PAGES_RELEASE_IDENTITY})" >&2; return 1; }
}

# pages_release_private_dir DIR — DIR is a directory owned by this user, mode
# 0700, and every directory above it is owned by root or this user and is
# either closed to group/other writes or sticky.
pages_release_private_dir() {
  local dir me owner mode p
  dir="$(readlink -f "$1")" || return 1
  me="$(id -u)"
  read -r owner mode < <(stat -c '%u %a' "$dir") || return 1
  if [[ ! -d "$dir" || -L "$dir" || "$owner" != "$me" || "$mode" != "700" ]]; then
    echo "staging dir $dir must be a directory owned by uid $me with mode 0700 — refusing it" >&2
    return 1
  fi
  p="$dir"
  while [[ "$p" != "/" ]]; do
    p="$(dirname "$p")"
    read -r owner mode < <(stat -c '%u %a' "$p") || return 1
    if [[ "$owner" != "0" && "$owner" != "$me" ]]; then
      echo "staging dir $dir sits under $p, owned by uid $owner — another user could replace it" >&2
      return 1
    fi
    if (( (8#$mode & 8#022) != 0 && (8#$mode & 8#1000) == 0 )); then
      echo "staging dir $dir sits under $p (mode $mode), which others can write to — they could replace it" >&2
      return 1
    fi
  done
}

# pages_release_older TAG VERSION — TAG (vYYYY.MM.DD.N) is older than the
# stamped VERSION a pages binary reports (v?YYYY.MM.DD.N[+…]).
pages_release_older() {
  local t="${1#v}" c="${2#v}"
  c="${c%%[+ ]*}"
  [[ "$c" =~ ^[0-9]{4}\.[0-9]{2}\.[0-9]{2}\.[0-9]+$ ]] || return 1
  [[ "$t" != "$c" && "$(printf '%s\n%s\n' "$t" "$c" | sort -V | head -n1)" == "$t" ]]
}

# pages_release_verify_manifest ROOT TAG COMMIT CACHE — fetch TAG's
# SHA256SUMS (+ signature) and release.json into a private temp dir and
# require: the signature by the release identity, release.json matching the
# signed sums, naming TAG, and naming COMMIT — the commit the tag resolves to
# in the local checkout.
pages_release_verify_manifest() {
  local root="$1" tag="$2" commit="$3" cache="$4" tmp cosign base name want got rc=0
  [[ "$tag" =~ ^v[0-9]{4}\.[0-9]{2}\.[0-9]{2}\.[1-9][0-9]*$ ]] || { echo "not a release tag: $tag" >&2; return 1; }
  [[ "$commit" =~ ^[0-9a-f]{40}$ ]] || { echo "not a commit: $commit" >&2; return 1; }
  cosign="$(pages_release_cosign "$root" "$cache")" || return 1
  tmp="$(mktemp -d)" || return 1
  base="$(pages_release_base_url)/${tag}"
  for name in SHA256SUMS SHA256SUMS.sigstore.json release.json; do
    pages_release_get "$base/$name" "$tmp/$name" "$PAGES_RELEASE_MAX_META" \
      || { echo "could not download ${name} for ${tag}" >&2; rc=1; break; }
  done
  if [[ "$rc" == 0 ]]; then
    pages_release_verify_blob "$cosign" "$tmp/SHA256SUMS" || rc=1
  fi
  if [[ "$rc" == 0 ]]; then
    want="$(awk '$2=="release.json" || $2=="*release.json" {print $1}' "$tmp/SHA256SUMS")"
    got="$(sha256sum "$tmp/release.json" | awk '{print $1}')"
    [[ ${#want} -eq 64 && "$want" == "$got" ]] || { echo "release.json does not match the signed SHA256SUMS — refusing it" >&2; rc=1; }
  fi
  if [[ "$rc" == 0 ]]; then
    grep -Eq "\"tag\"[[:space:]]*:[[:space:]]*\"${tag//./\\.}\"" "$tmp/release.json" \
      || { echo "release.json does not name ${tag} — refusing a release served under another tag" >&2; rc=1; }
  fi
  if [[ "$rc" == 0 ]]; then
    grep -Eq "\"commit\"[[:space:]]*:[[:space:]]*\"${commit}\"" "$tmp/release.json" \
      || { echo "the signed release.json does not name commit ${commit} for ${tag} — the tag in this checkout was moved; refusing it" >&2; rc=1; }
  fi
  rm -rf "$tmp"
  return "$rc"
}

# pages_release_fetch ROOT TAG DIR CACHE [COMMIT] — download and verify TAG's
# binary for this box; copy the verified files into DIR (a private dir, see
# pages_release_private_dir) and print the verified binary's path there.
pages_release_fetch() {
  local root="$1" tag="$2" dir="$3" cache="$4" commit="${5:-}" arch cosign base name want got _max tmp rc=0 cur cur_tag newest
  [[ "$tag" =~ ^v[0-9]{4}\.[0-9]{2}\.[0-9]{2}\.[1-9][0-9]*$ ]] || { echo "not a release tag: $tag" >&2; return 1; }
  cur="${PAGES_CURRENT_VERSION:-}"
  cur="${cur%%+*}"
  if [[ -n "$cur" && "${PAGES_ALLOW_DOWNGRADE:-0}" != "1" && "$cur" =~ ^v?[0-9]{4}\.[0-9]{2}\.[0-9]{2}\.[1-9][0-9]*$ ]]; then
    cur_tag="v${cur#v}"
    newest="$(printf '%s\n%s\n' "$cur_tag" "$tag" | sort -t. -k1.2,1n -k2,2n -k3,3n -k4,4n | tail -n 1)"
    if [[ "$newest" != "$tag" ]]; then
      echo "downgrade refused: ${tag} is older than the running ${cur_tag} (pass --allow-downgrade to roll back on purpose)" >&2
      return 1
    fi
  fi
  arch="$(pages_release_arch)" || return 1
  cosign="$(pages_release_cosign "$root" "$cache")" || return 1
  base="$(pages_release_base_url)/${tag}"
  install -d -m 0700 "$dir" || return 1
  chmod 0700 "$dir" || return 1
  pages_release_private_dir "$dir" || return 1
  tmp="$(mktemp -d)" || return 1
  for name in SHA256SUMS SHA256SUMS.sigstore.json "pages-linux-${arch}" "pages-linux-${arch}.sigstore.json" release.json; do
    _max="$PAGES_RELEASE_MAX_META"
    [[ "$name" == "pages-linux-${arch}" ]] && _max="$PAGES_RELEASE_MAX_BINARY"
    if ! pages_release_get "$base/$name" "$tmp/$name" "$_max"; then
      echo "could not download ${name} for ${tag}" >&2
      rc=1; break
    fi
  done
  [[ "$rc" == 0 ]] && { pages_release_verify_blob "$cosign" "$tmp/SHA256SUMS" || rc=1; }
  if [[ "$rc" == 0 ]]; then
    for name in "pages-linux-${arch}" release.json; do
      want="$(awk -v n="$name" '$2==n || $2=="*"n {print $1}' "$tmp/SHA256SUMS")"
      got="$(sha256sum "$tmp/$name" | awk '{print $1}')"
      if [[ ${#want} -ne 64 || "$want" != "$got" ]]; then
        echo "${name} does not match the signed SHA256SUMS — refusing it" >&2
        rc=1; break
      fi
    done
  fi
  if [[ "$rc" == 0 ]]; then
    grep -Eq "\"tag\"[[:space:]]*:[[:space:]]*\"${tag//./\\.}\"" "$tmp/release.json" \
      || { echo "release.json does not name ${tag} — refusing a release served under another tag" >&2; rc=1; }
  fi
  if [[ "$rc" == 0 && -n "$commit" ]]; then
    grep -Eq "\"commit\"[[:space:]]*:[[:space:]]*\"${commit}\"" "$tmp/release.json" \
      || { echo "the signed release.json does not name commit ${commit} for ${tag} — refusing it" >&2; rc=1; }
  fi
  [[ "$rc" == 0 ]] && { pages_release_verify_blob "$cosign" "$tmp/pages-linux-${arch}" || rc=1; }
  if [[ "$rc" == 0 ]]; then
    chmod 0755 "$tmp/pages-linux-${arch}"
    for name in SHA256SUMS SHA256SUMS.sigstore.json "pages-linux-${arch}" "pages-linux-${arch}.sigstore.json" release.json; do
      cp -f --no-preserve=ownership "$tmp/$name" "$dir/$name" || { rc=1; break; }
    done
    if [[ "$rc" == 0 ]]; then
      chmod 0600 "$dir"/SHA256SUMS* "$dir"/release.json "$dir/pages-linux-${arch}.sigstore.json"
      chmod 0755 "$dir/pages-linux-${arch}"
      want="$(awk -v n="pages-linux-${arch}" '$2==n || $2=="*"n {print $1}' "$tmp/SHA256SUMS")"
      [[ "$(sha256sum "$dir/pages-linux-${arch}" | awk '{print $1}')" == "$want" ]] && pages_release_private_dir "$dir" || rc=1
    fi
  fi
  rm -rf "$tmp"
  [[ "$rc" == 0 ]] || return 1
  printf '%s' "$dir/pages-linux-${arch}"
}
