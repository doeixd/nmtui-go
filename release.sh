#!/bin/bash

set -e # Exit immediately if a command exits with a non-zero status.

# --- Configuration ---
DEFAULT_BRANCH="main" # Or "master", or your primary development branch
REMOTE_NAME="origin"  # Your Git remote name

# --- Helper Functions ---
get_latest_tag() {
  # Gets the latest semantic version tag. Handles initial case where no tags exist.
  git describe --tags --abbrev=0 2>/dev/null || echo "v0.0.0"
}

increment_version() {
  local version=$1
  local part=$2 # major, minor, patch
  
  # Remove 'v' prefix for manipulation
  version_no_v="${version#v}"
  
  # Split into parts
  IFS='.' read -r -a parts <<< "$version_no_v"
  local major=${parts[0]:-0}
  local minor=${parts[1]:-0}
  local patch=${parts[2]:-0}

  case "$part" in
    major)
      major=$((major + 1))
      minor=0
      patch=0
      ;;
    minor)
      minor=$((minor + 1))
      patch=0
      ;;
    patch)
      patch=$((patch + 1))
      ;;
    *)
      echo "Error: Invalid version part '$part'. Use 'major', 'minor', or 'patch'." >&2
      exit 1
      ;;
  esac
  echo "v${major}.${minor}.${patch}"
}

# --- Main Script ---

echo "Starting release process..."

# 1. Check for clean Git working directory
if ! git diff-index --quiet HEAD --; then
  echo "Error: Your working directory is not clean. Please commit or stash your changes." >&2
  exit 1
fi
echo "✓ Git working directory is clean."

# 2. Ensure we are on the default branch
current_branch=$(git rev-parse --abbrev-ref HEAD)
if [ "$current_branch" != "$DEFAULT_BRANCH" ]; then
  echo "Warning: You are not on the '$DEFAULT_BRANCH' branch (currently on '$current_branch')."
  read -p "Switch to '$DEFAULT_BRANCH' and pull latest? (y/N): " switch_branch_confirm
  if [[ "$switch_branch_confirm" =~ ^[Yy]$ ]]; then
    git checkout "$DEFAULT_BRANCH"
    git pull "$REMOTE_NAME" "$DEFAULT_BRANCH"
  else
    echo "Aborting release. Please switch to the '$DEFAULT_BRANCH' branch manually."
    exit 1
  fi
fi
echo "✓ On branch '$DEFAULT_BRANCH'."

# 3. Fetch latest tags from remote
echo "Fetching latest tags from remote '$REMOTE_NAME'..."
git fetch "$REMOTE_NAME" --tags
echo "✓ Fetched tags."

# 4. Get the latest tag
latest_tag=$(get_latest_tag)
echo "Latest existing tag: $latest_tag"

# 5. Prompt for version bump type or specific version
echo ""
echo "How do you want to version this release?"
echo "  1) Patch (e.g., $latest_tag -> $(increment_version "$latest_tag" "patch"))"
echo "  2) Minor (e.g., $latest_tag -> $(increment_version "$latest_tag" "minor"))"
echo "  3) Major (e.g., $latest_tag -> $(increment_version "$latest_tag" "major"))"
echo "  4) Enter specific version (e.g., v1.2.3)"
echo "  q) Quit"

read -p "Choose an option (1-4, q): " choice

new_version=""

case "$choice" in
  1) new_version=$(increment_version "$latest_tag" "patch") ;;
  2) new_version=$(increment_version "$latest_tag" "minor") ;;
  3) new_version=$(increment_version "$latest_tag" "major") ;;
  4)
    read -p "Enter the new version (e.g., v1.2.3): " specific_version
    if [[ ! "$specific_version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9.-]+)?$ ]]; then # Basic SemVer check + pre-release
      echo "Error: Invalid version format. Expected format like v1.2.3 or v1.2.3-beta.1" >&2
      exit 1
    fi
    new_version=$specific_version
    ;;
  q|Q)
    echo "Release process aborted by user."
    exit 0
    ;;
  *)
    echo "Error: Invalid choice." >&2
    exit 1
    ;;
esac

echo "Selected new version: $new_version"

# 6. Confirm before proceeding
echo ""
read -p "Are you sure you want to create and push tag '$new_version'? (y/N): " confirm_tag
if [[ ! "$confirm_tag" =~ ^[Yy]$ ]]; then
  echo "Release aborted by user."
  exit 0
fi

# 7. Create an annotated Git tag
echo "Creating annotated tag '$new_version'..."
# You can customize the commit message for the release commit if you do one.
# For simplicity, this script just tags the current HEAD.
# If you have a CHANGELOG.md, you might want to commit it first with a "chore: release $new_version" message.
# For now, let's assume the current HEAD is the release candidate.
tag_message="Release version $new_version" # Message for the annotated tag
git tag -a "$new_version" -m "$tag_message"
echo "✓ Tag '$new_version' created locally."

# 8. Push the tag to the remote
echo "Pushing tag '$new_version' to remote '$REMOTE_NAME'..."
git push "$REMOTE_NAME" "$new_version"
echo "✓ Tag '$new_version' pushed to remote."

echo ""
echo "------------------------------------------------------------------"
echo "Release process for $new_version initiated!"
echo "The GitHub Actions workflow should now start building and publishing the release."
echo "Monitor its progress on your GitHub repository's 'Actions' tab."
echo "------------------------------------------------------------------"

exit 0
