// SPDX-License-Identifier: AGPL-3.0-or-later

// Package epg handles XMLTV files. It downloads one and lists its channel
// ids for epg ids (spec 7.2), reading only the channel list. In EPG proxy
// mode (spec 6.1) Guide fetches the [[epg.source]] files through their
// routes, Filter keeps the configured channels and their programmes as a
// stream and writes them again from the parsed tokens, and Merge builds
// the served guide.
package epg
