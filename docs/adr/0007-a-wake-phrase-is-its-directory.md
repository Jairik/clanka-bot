# A wake phrase is its directory; "trained" is what the wakeword service advertises

Issue #150 asked for a custom Wake phrase to persist the moment it is added on `/voice-samples`, and for a mark on every phrase still missing samples or a trained model. That needed two decisions: where an added phrase lives, and how the Assistant knows a phrase is trained.

**Where a phrase lives.** Adding a phrase creates its empty `<slug>/real/{positive,negative}` directories in the samples volume. There is no registry file. The store already listed every valid slug directory, and phrases recorded before this change are already directories, so the directory stays the single record. A registry file would be a second record that could disagree with the directories (a phrase with takes but no entry, or an entry with no directory), and it would need a migration on the Pi's live volume. Removal deletes only those known directories with `os.Remove`, never recursively, so any file left in the tree makes removal fail instead of deleting a take.

**What "trained" means.** A phrase is trained when the wakeword service's Wyoming `describe` answer advertises its model name: the slug with dashes replaced by underscores, which is the name the desktop trainer gives it. This is the same probe the Health page uses. The Dashboard cannot see the desktop's `wakeword/models/`, and a file there is not served until the image is rebuilt and redeployed. When the service is not configured or does not answer, model status is "unknown". The page says so once, and only the sample gaps are flagged.

## Consequences

- Clearing every take keeps the phrase listed (still marked `!`). Only Remove phrase makes it disappear.
- An old `?phrase=` link to an unstored phrase still opens and records. It is shown as "not saved yet" until its first take.
- The voice-samples view caches the `describe` answer for 30 s (10 s after a failure), so the batch recorder's refresh after each take does not each wait on the service. The Health page still probes live.
