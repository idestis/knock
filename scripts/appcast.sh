#!/usr/bin/env bash
# appcast.sh <dmg> <version> <build> <tag> <owner/repo> <sparkle-bin-dir> <sparkle-account>
# Prints a Sparkle appcast for one release. It is uploaded to every GitHub
# release, so releases/latest/download/appcast.xml always names the newest DMG.
set -euo pipefail
dmg=$1 version=$2 build=$3 tag=$4 repo=$5 bin=$6 account=$7
sig=$("$bin/sign_update" --account "$account" "$dmg")   # sparkle:edSignature="…" length="…"
min=$(/usr/libexec/PlistBuddy -c 'Print :LSMinimumSystemVersion' "$(dirname "$dmg")/Knock.app/Contents/Info.plist" 2>/dev/null || echo 26.0)
notes=$(git cliff --config "$(dirname "$0")/cliff-sparkle.toml" --current 2>/dev/null || true)
[ -n "$notes" ] || notes="<p>Knock $version</p>"
url="https://github.com/$repo/releases/download/$tag/$(basename "$dmg")"
cat <<XML
<?xml version="1.0" encoding="utf-8"?>
<rss version="2.0" xmlns:sparkle="http://www.andymatuschak.org/xml-namespaces/sparkle">
  <channel>
    <title>Knock</title>
    <link>https://github.com/$repo</link>
    <item>
      <title>Knock $version</title>
      <pubDate>$(LC_ALL=C date -u '+%a, %d %b %Y %H:%M:%S +0000')</pubDate>
      <sparkle:version>$build</sparkle:version>
      <sparkle:shortVersionString>$version</sparkle:shortVersionString>
      <sparkle:minimumSystemVersion>$min</sparkle:minimumSystemVersion>
      <sparkle:releaseNotesLink>https://github.com/$repo/releases/tag/$tag</sparkle:releaseNotesLink>
      <description><![CDATA[$notes]]></description>
      <enclosure url="$url" type="application/x-apple-diskimage" $sig />
    </item>
  </channel>
</rss>
XML
