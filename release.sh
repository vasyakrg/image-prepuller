#!/usr/bin/env bash
#
# release.sh — cut a release of image-prepuller.
#
# The project keeps exactly one version line: Chart.yaml version == appVersion ==
# internal/agent.Version == release tag (vX.Y.Z -> X.Y.Z). This script bumps all
# three in one commit, closes the CHANGELOG's Unreleased section, runs the local
# gates, commits "release: vX.Y.Z", creates the annotated tag and pushes main and
# the tag. The tag is what CI publishes from: gates test+helm, then the
# multi-arch image, then (tag only) the OCI chart.
#
# Usage:
#   ./release.sh 1.1.0 [-m "short description"] [-y]
#
# Options:
#   -m, --message TEXT   tag message after the version ("v1.1.0: TEXT")
#       --remote NAME    git remote to push to (default: origin)
#       --branch NAME    branch to release from (default: main)
#       --no-changelog   leave CHANGELOG.md untouched
#       --skip-tests     skip "go test ./..." (build/vet/gofmt and helm still run)
#       --no-push        commit and tag locally, push nothing
#   -n, --dry-run        print the plan, change nothing, push nothing
#   -y, --yes            never ask for confirmation
#   -h, --help           this help
#
# Examples:
#   ./release.sh 1.1.0 -m "Stats tab in hub UI"
#   ./release.sh 1.2.0 --dry-run
#
# After the publish finishes, record the image/chart digests in
# docs/DEPLOY.md section 13 and bump the install examples in README.md.
set -euo pipefail

PROG="$(basename "$0")"

die()  { printf '%s: error: %s\n' "$PROG" "$*" >&2; exit 1; }
info() { printf '>> %s\n' "$*"; }
warn() { printf '%s: warning: %s\n' "$PROG" "$*" >&2; }

usage() {
	sed -n '3,33p' "$0" | sed 's/^# \{0,1\}//'
}

VERSION=""
TAG_MESSAGE=""
REMOTE="origin"
BRANCH="main"
DRY_RUN=0
ASSUME_YES=0
DO_CHANGELOG=1
SKIP_TESTS=0
PUSH=1

while [ $# -gt 0 ]; do
	case "$1" in
		-m | --message)
			[ $# -ge 2 ] || die "$1 needs an argument"
			TAG_MESSAGE="$2"; shift 2 ;;
		--remote)
			[ $# -ge 2 ] || die "$1 needs an argument"
			REMOTE="$2"; shift 2 ;;
		--branch)
			[ $# -ge 2 ] || die "$1 needs an argument"
			BRANCH="$2"; shift 2 ;;
		--no-changelog) DO_CHANGELOG=0; shift ;;
		--skip-tests)   SKIP_TESTS=1; shift ;;
		--no-push)      PUSH=0; shift ;;
		-n | --dry-run) DRY_RUN=1; shift ;;
		-y | --yes)     ASSUME_YES=1; shift ;;
		-h | --help)    usage; exit 0 ;;
		-*)             die "unknown option $1 (try --help)" ;;
		*)
			[ -z "$VERSION" ] || die "version given twice ($VERSION, $1)"
			VERSION="$1"; shift ;;
	esac
done

[ -n "$VERSION" ] || { usage; exit 2; }

# Accept v1.2.3 and 1.2.3; the tag always gets exactly one "v".
VERSION="${VERSION#v}"
printf '%s' "$VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' ||
	die "version must look like X.Y.Z, got '$VERSION'"

TAG="v$VERSION"
ROOT="$(git rev-parse --show-toplevel 2>/dev/null)" || die "not inside a git repository"
cd "$ROOT"

CHART="deploy/helm/image-prepuller/Chart.yaml"
AGENT_GO="internal/agent/agent.go"
CHANGELOG="CHANGELOG.md"
for f in "$CHART" "$AGENT_GO" "$CHANGELOG"; do
	[ -f "$f" ] || die "missing $f (run from the image-prepuller checkout)"
done

# --- current state ---------------------------------------------------------

current_version="$(awk '$1 == "version:" { print $2; exit }' "$CHART")"
current_app_version="$(awk '$1 == "appVersion:" { gsub(/"/, "", $2); print $2; exit }' "$CHART")"
current_agent_version="$(awk '$1 == "const" && $2 == "Version" { gsub(/"/, "", $4); print $4; exit }' "$AGENT_GO")"

[ -n "$current_version" ] || die "cannot read version from $CHART"
[ -n "$current_app_version" ] || die "cannot read appVersion from $CHART"
[ -n "$current_agent_version" ] || die "cannot read const Version from $AGENT_GO"

if [ "$current_version" != "$current_app_version" ]; then
	die "chart is already inconsistent: version=$current_version appVersion=$current_app_version"
fi

# Strictly increasing versions only: the release line never goes down.
version_gt() { # $1 > $2
	awk -v a="$1" -v b="$2" 'BEGIN {
		na = split(a, A, "."); nb = split(b, B, ".")
		if (na != 3 || nb != 3) exit 2
		for (i = 1; i <= 3; i++) {
			if (A[i] + 0 > B[i] + 0) exit 0
			if (A[i] + 0 < B[i] + 0) exit 1
		}
		exit 1
	}'
}
version_gt "$VERSION" "$current_version" ||
	die "$TAG is not newer than the current version line ($current_version)"

branch_now="$(git rev-parse --abbrev-ref HEAD)"
[ "$branch_now" = "$BRANCH" ] ||
	die "on branch '$branch_now', releases are cut from '$BRANCH' (use --branch to override)"

# A release commit must contain only the release itself.
if [ -n "$(git status --porcelain)" ]; then
	git status --short >&2
	die "working tree is dirty — commit or stash first (the release commit must be clean)"
fi

info "repository:    $ROOT"
info "branch:        $branch_now"
info "version line:  $current_version -> $VERSION (tag $TAG)"

# Documentation usually pins the previous release in install examples.
old_refs="$(git grep -l --fixed-strings "$current_version" -- '*.md' '*.yaml' '*.yml' '*.sh' 2>/dev/null |
	grep -v -e "^$CHANGELOG$" -e "^$CHART$" || true)"
if [ -n "$old_refs" ]; then
	warn "these files still mention $current_version — update them before tagging:"
	printf '   %s\n' $old_refs >&2
fi

# --- remote checks ---------------------------------------------------------

if git rev-parse -q --verify "refs/tags/$TAG" >/dev/null; then
	die "tag $TAG already exists locally"
fi

if [ "$PUSH" -eq 1 ]; then
	git fetch --quiet --tags "$REMOTE" "$BRANCH" ||
		die "cannot fetch $REMOTE — check the network/credentials (or use --no-push)"
	if [ -n "$(git ls-remote --tags "$REMOTE" "refs/tags/$TAG")" ]; then
		die "tag $TAG already exists on $REMOTE"
	fi
	head_sha="$(git rev-parse HEAD)"
	remote_sha="$(git rev-parse "$REMOTE/$BRANCH")"
	if [ "$head_sha" != "$remote_sha" ]; then
		die "$BRANCH is not in sync with $REMOTE/$BRANCH — pull/rebase first (push would be rejected)"
	fi
else
	info "no-push mode: skipping remote checks"
fi

# --- changelog -------------------------------------------------------------

date_utc="$(date -u +%Y-%m-%d)"
changelog_unreleased=0
if [ "$DO_CHANGELOG" -eq 1 ] && grep -q '^## \[Unreleased\]' "$CHANGELOG"; then
	# Only close the section when it actually has content below the header.
	if awk '
		/^## \[Unreleased\]/ { in_section = 1; next }
		in_section && /^## \[/ { exit 1 }
		in_section && NF { found = 1 }
		END { exit(found ? 0 : 1) }
	' "$CHANGELOG"; then
		changelog_unreleased=1
		info "CHANGELOG: closing [Unreleased] as [$VERSION] — $date_utc"
	else
		warn "CHANGELOG: [Unreleased] is empty, leaving it alone"
	fi
fi

# --- plan ------------------------------------------------------------------

echo
info "planned changes:"
echo "   $CHART: version $current_version -> $VERSION, appVersion $current_app_version -> $VERSION"
echo "   $AGENT_GO: const Version $current_agent_version -> $VERSION"
if [ "$changelog_unreleased" -eq 1 ]; then
	echo "   $CHANGELOG: '## [Unreleased]' -> '## [$VERSION] — $date_utc'"
fi
echo "   commit: 'release: $TAG'"
echo "   tag:    $TAG (annotated, message '${TAG_MESSAGE:-Release $TAG}')"
if [ "$PUSH" -eq 1 ]; then
	echo "   push:   $REMOTE $BRANCH, then $REMOTE $TAG"
fi
echo

if [ "$DRY_RUN" -eq 1 ]; then
	info "dry run: nothing changed"
	exit 0
fi

if [ "$ASSUME_YES" -eq 0 ]; then
	[ -t 0 ] || die "non-interactive shell: pass --yes to confirm"
	printf 'Cut %s now? [y/N] ' "$TAG"
	read -r answer
	case "$answer" in
		y | Y | yes | YES) ;;
		*) die "aborted" ;;
	esac
fi

# --- bump ------------------------------------------------------------------

chart_tmp="$(mktemp)"
agent_tmp="$(mktemp)"
changelog_tmp="$(mktemp)"
trap 'rm -f "$chart_tmp" "$agent_tmp" "$changelog_tmp"' EXIT

awk -v v="$VERSION" '
	$1 == "version:"    { print "version: " v; next }
	$1 == "appVersion:" { print "appVersion: \"" v "\""; next }
	{ print }
' "$CHART" >"$chart_tmp"

awk -v v="$VERSION" '
	$1 == "const" && $2 == "Version" && $3 == "=" {
		print "const Version = \"" v "\""
		next
	}
	{ print }
' "$AGENT_GO" >"$agent_tmp"

mv "$chart_tmp" "$CHART"
mv "$agent_tmp" "$AGENT_GO"

if [ "$changelog_unreleased" -eq 1 ]; then
	awk -v v="$VERSION" -v d="$date_utc" '
		!done && /^## \[Unreleased\]/ {
			print "## [" v "] — " d
			done = 1
			next
		}
		{ print }
	' "$CHANGELOG" >"$changelog_tmp"
	mv "$changelog_tmp" "$CHANGELOG"
fi
trap - EXIT
rm -f "$chart_tmp" "$agent_tmp" "$changelog_tmp"

# Read the values back: a release that silently bumps nothing is worse than a
# failed push.
[ "$(awk '$1 == "version:" { print $2; exit }' "$CHART")" = "$VERSION" ] ||
	die "failed to bump version in $CHART"
[ "$(awk '$1 == "appVersion:" { gsub(/"/, "", $2); print $2; exit }' "$CHART")" = "$VERSION" ] ||
	die "failed to bump appVersion in $CHART"
[ "$(awk '$1 == "const" && $2 == "Version" { gsub(/"/, "", $4); print $4; exit }' "$AGENT_GO")" = "$VERSION" ] ||
	die "failed to bump const Version in $AGENT_GO"

# --- gates -----------------------------------------------------------------

if command -v go >/dev/null 2>&1; then
	info "gofmt"
	unformatted="$(gofmt -l cmd internal)"
	[ -z "$unformatted" ] || die "gofmt would rewrite: $unformatted"

	info "go build ./..."
	go build ./...

	info "go vet ./..."
	go vet ./...

	if [ "$SKIP_TESTS" -eq 1 ]; then
		warn "skipping go test ./... (--skip-tests)"
	else
		info "go test ./..."
		go test ./...
	fi
else
	warn "go not found — skipping build/vet/test"
fi

if command -v helm >/dev/null 2>&1; then
	info "helm lint"
	helm lint deploy/helm/image-prepuller >/dev/null

	info "helm template (defaults)"
	helm template prepuller deploy/helm/image-prepuller >/dev/null
else
	warn "helm not found — skipping chart gates"
fi

# --- commit, tag, push -----------------------------------------------------

info "committing"
git add -- "$CHART" "$AGENT_GO"
if [ "$changelog_unreleased" -eq 1 ]; then
	git add -- "$CHANGELOG"
fi

commit_body="- Chart.yaml version/appVersion $VERSION (chart and app share one line;
  the release tag v$VERSION must match both, CI fails the tag otherwise).
- internal/agent.Version $VERSION: \`prepuller agent -version\` and
  prepuller_agent_build_info report the same number."
if [ "$changelog_unreleased" -eq 1 ]; then
	commit_body="$commit_body
- CHANGELOG: [Unreleased] closed as [$VERSION] — $date_utc."
fi
git commit -q -m "release: $TAG" -m "$commit_body"
release_sha="$(git rev-parse --short=12 HEAD)"
info "release commit: $release_sha"

if [ -n "$TAG_MESSAGE" ]; then
	git tag -a "$TAG" -m "$TAG: $TAG_MESSAGE"
else
	git tag -a "$TAG" -m "Release $TAG"
fi
info "tagged $TAG"

if [ "$PUSH" -eq 1 ]; then
	info "pushing $BRANCH to $REMOTE"
	git push "$REMOTE" "$BRANCH"
	info "pushing tag $TAG"
	git push "$REMOTE" "refs/tags/$TAG"
fi

# --- next steps ------------------------------------------------------------

echo
info "$TAG is cut at $release_sha."
if [ "$PUSH" -eq 1 ]; then
	echo "CI now runs gates -> image :sha-$release_sha, :$VERSION (amd64+arm64) -> OCI chart $VERSION."
else
	echo "Nothing was pushed: 'git push $REMOTE $BRANCH && git push $REMOTE refs/tags/$TAG' when ready."
fi
cat <<EOF

  image: ghcr.io/vasyakrg/image-prepuller:$VERSION
  chart: oci://ghcr.io/vasyakrg/charts/image-prepuller --version $VERSION

Follow-ups once the packages are published:
  1. Record the image/chart digests in docs/DEPLOY.md section 13.
  2. Bump the pinned versions in README.md install examples.
  3. Roll the stand: pin image.tag=$release_sha (or $VERSION once the chart is out).
EOF
