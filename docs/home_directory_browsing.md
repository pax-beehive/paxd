# Home directory browsing

The authenticated control query `directories.list` accepts `list_directories`
with optional `path`, `after` and `show_hidden`. Empty path starts at the home of
the OS account running paxd. The query returns `directories` containing `home`,
`path`, optional `parent`, `entries` (name/path) and optional `next_after`.

All filesystem access uses an `os.Root` handle opened at that home. Absolute paths
must be lexically inside home, and the handle prevents symlink traversal outside
it. Links to directories outside home are omitted. File contents are never read,
and children are not traversed until requested. Existing session execution paths
are unchanged by this read-only browsing feature.

Each request scans only the current directory in bounded batches, retaining up
to 101 lexically ordered candidates. At most 100 folders are returned. A nonempty
`next_after` is passed as `after` for the next page. Hidden folders default off.
Changes during pagination can alter results; restarting at an empty cursor gives
a fresh view. No persistent cache, index, or periodic filesystem upload is added.

Directory tests cover boundary traversal, outside and inside symlinks, hidden
folders, omission of ordinary files, page continuation, cancellation, validation
and authenticated sources. The existing startup/10-second version heartbeat and
idle-mode verified upgrade machinery are unchanged. Native clients consume those
existing contracts to display and upgrade paxd.

No binary release or live-node installation has been performed for this change.
Manager and native gateway support is in the sibling pax-manager-android-native
checkout.
