# packaging/homebrew/caravan.rb — Homebrew Formula template
#
# HOW TO PUBLISH THIS FORMULA
# ============================================================
# 1. Create a GitHub repository for caravan (e.g. github.com/YOU/caravan).
# 2. Tag a release (e.g. git tag v0.2.0 && git push --tags).
#    GitHub Actions or `make release` produces the .tar.gz source archive.
# 3. Replace the two REPLACE_ME placeholders below:
#      url  → the GitHub source tarball URL, e.g.:
#             https://github.com/YOU/caravan/archive/refs/tags/v0.2.0.tar.gz
#      sha256 → sha256 of that tarball (shasum -a 256 <downloaded-tarball>)
# 4. To host a personal Homebrew tap:
#      a. Create github.com/YOU/homebrew-tap
#      b. Drop this file at Formula/caravan.rb in that repo
#      c. Users install with:
#           brew tap YOU/tap
#           brew install caravan
#    Or submit to homebrew-core once the project meets their criteria.
# ============================================================

class Caravan < Formula
  desc "Drive for devs: one manifest, identical dev workspaces on every machine"
  homepage "https://github.com/REPLACE_ME/caravan"
  url "https://github.com/REPLACE_ME/caravan/archive/refs/tags/v0.2.0.tar.gz"
  sha256 "REPLACE_ME_SHA256"
  license "MIT"

  # Homebrew uses its own Go toolchain; no separate dep needed.
  depends_on "go" => :build

  def install
    # Build a fully static binary (no CGO)
    ENV["CGO_ENABLED"] = "0"
    system "go", "build",
           "-trimpath",
           "-ldflags", "-s -w",
           "-o", bin/"caravan",
           "."
  end

  test do
    # `caravan version` must exit 0 and print the version string
    assert_match "caravan 0.2.0", shell_output("#{bin}/caravan version")
  end
end
