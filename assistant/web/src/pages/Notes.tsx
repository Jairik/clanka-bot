import { useEffect, useState } from "react";
import { Button, Checkbox } from "@heroui/react";
import { Pencil, Plus, StickyNote, Trash2, X } from "lucide-react";
import { usePageData } from "../api";
import { Field, TextField, useActions, type Actions } from "../forms";
import { PageFrame } from "../shell";
import type { NoteItem, NoteRow, NotesList, NotesView } from "../types";
import { Empty, Lede, Notice, Section, cn } from "../ui";

export function NotesPage() {
  const { data, error, set } = usePageData("notes");
  const actions = useActions<NotesView>(set);
  return (
    <PageFrame id="notes">
      <Notice error={error || actions.error} flash={actions.flash} onDismiss={actions.clear} />
      {data && !data.HasStore && (
        <Section title="Notes are not configured" defaultOpen>
          <Empty>This Dashboard was built without the Note and List modules, so there is nothing to show.</Empty>
        </Section>
      )}
      {data?.HasStore && (
        <div className="space-y-2.5">
          {(data.Lists ?? []).map((list) => (
            <ListSection key={list.Key} list={list} actions={actions} />
          ))}
          <NotesSection notes={data.Notes ?? []} actions={actions} />
        </div>
      )}
    </PageFrame>
  );
}

function ListSection({ list, actions }: { list: NotesList; actions: Actions<NotesView> }) {
  const [text, setText] = useState("");
  const save = async () => {
    if (!text.trim()) return;
    if (await actions.run(list.AddAction, { text })) setText("");
  };
  return (
    <Section title={`${list.Title} list`} summary={`${list.Open} open`} defaultOpen flush>
      <Lede>A fixed list. Tick an item when it is done, or remove it for good.</Lede>
      {list.Items?.length ? (
        <ul className="divide-y divide-separator">
          {list.Items.map((item) => (
            <ListItem key={item.ID} item={item} actions={actions} />
          ))}
        </ul>
      ) : (
        <Empty>Nothing on the {list.Title.toLowerCase()} list.</Empty>
      )}
      <form
        className="flex items-end gap-2 border-t px-3.5 py-3"
        onSubmit={(e) => {
          e.preventDefault();
          void save();
        }}
      >
        <Field label={`Add to the ${list.Title.toLowerCase()} list`} className="min-w-0 flex-1">
          <TextField label="New item" value={text} onChange={setText} placeholder="Add an item…" />
        </Field>
        <Button size="sm" type="submit" isPending={actions.pending === list.AddAction} isDisabled={!text.trim()}>
          <Plus className="size-3.5" />
          Add
        </Button>
      </form>
    </Section>
  );
}

function ListItem({ item, actions }: { item: NoteItem; actions: Actions<NotesView> }) {
  const [editing, setEditing] = useState(false);
  const [text, setText] = useState(item.Text);
  useEffect(() => {
    setText(item.Text);
    setEditing(false);
  }, [item.Text]);

  return (
    <li>
      <div className={cn("flex min-h-12 items-center gap-2.5 px-3.5 py-2", item.Done && "text-muted")}>
        <Checkbox
          isSelected={item.Done}
          onChange={() => void actions.run(item.ToggleAction)}
          isDisabled={actions.pending === item.ToggleAction}
          aria-label={`${item.Done ? "Untick" : "Tick"} ${item.Text}`}
        >
          <Checkbox.Content>
            <Checkbox.Control>
              <Checkbox.Indicator />
            </Checkbox.Control>
          </Checkbox.Content>
        </Checkbox>
        <span className={cn("min-w-0 flex-1 break-words text-sm", item.Done && "line-through")}>{item.Text}</span>
        <Button
          isIconOnly
          size="sm"
          variant="ghost"
          aria-label={`Edit ${item.Text}`}
          onPress={() => setEditing((v) => !v)}
        >
          <Pencil className="size-3.5" />
        </Button>
        <Button
          isIconOnly
          size="sm"
          variant="danger-soft"
          aria-label={`Remove ${item.Text}`}
          isPending={actions.pending === item.RemoveAction}
          onPress={() => void actions.run(item.RemoveAction)}
        >
          <X className="size-3.5" />
        </Button>
      </div>
      {editing && (
        <form
          className="flex items-end gap-2 border-t border-dashed bg-surface-secondary px-3.5 py-3"
          onSubmit={(e) => {
            e.preventDefault();
            void actions.run(item.EditAction, { text });
          }}
        >
          <Field label="Edit item" className="min-w-0 flex-1">
            <TextField label="Item text" value={text} onChange={setText} />
          </Field>
          <Button size="sm" type="submit" isPending={actions.pending === item.EditAction} isDisabled={!text.trim() || text === item.Text}>
            Save
          </Button>
        </form>
      )}
    </li>
  );
}

function NotesSection({ notes, actions }: { notes: NoteRow[]; actions: Actions<NotesView> }) {
  const [text, setText] = useState("");
  const save = async () => {
    if (!text.trim()) return;
    if (await actions.run("/notes", { text })) setText("");
  };
  return (
    <Section title="Notes" summary={`${notes.length} remembered`} defaultOpen flush>
      <Lede>Ask clanka to remember something, or keep it here. Recall searches by meaning, so the exact words do not matter.</Lede>
      <form
        className="space-y-2 border-b px-3.5 py-3"
        onSubmit={(e) => {
          e.preventDefault();
          void save();
        }}
      >
        <Field label="New note">
          <TextField label="Note text" value={text} onChange={setText} multiline rows={2} placeholder="Remember that…" />
        </Field>
        <div className="flex justify-end">
          <Button size="sm" type="submit" isPending={actions.pending === "/notes"} isDisabled={!text.trim()}>
            <Plus className="size-3.5" />
            Save note
          </Button>
        </div>
      </form>
      {notes.length ? (
        <ul className="divide-y divide-separator">
          {notes.map((note) => (
            <NoteItemRow key={note.ID} note={note} actions={actions} />
          ))}
        </ul>
      ) : (
        <Empty>No notes yet.</Empty>
      )}
    </Section>
  );
}

function NoteItemRow({ note, actions }: { note: NoteRow; actions: Actions<NotesView> }) {
  const [editing, setEditing] = useState(false);
  const [text, setText] = useState(note.Text);
  useEffect(() => {
    setText(note.Text);
    setEditing(false);
  }, [note.Text]);

  return (
    <li className="px-3.5 py-2.5">
      <div className="flex items-start gap-2.5">
        <StickyNote className="mt-0.5 size-4 shrink-0 text-muted" strokeWidth={1.5} />
        <div className="min-w-0 flex-1">
          <p className="break-words text-sm">{note.Text}</p>
          <time dateTime={note.UpdatedISO} className="mt-0.5 block text-[11px] text-muted">
            Updated {note.Updated}
          </time>
        </div>
        <Button isIconOnly size="sm" variant="ghost" aria-label={`Edit note ${note.Text}`} onPress={() => setEditing((v) => !v)}>
          <Pencil className="size-3.5" />
        </Button>
        <Button
          isIconOnly
          size="sm"
          variant="danger-soft"
          aria-label={`Delete note ${note.Text}`}
          isPending={actions.pending === note.DeleteAction}
          onPress={() => void actions.run(note.DeleteAction)}
        >
          <Trash2 className="size-3.5" />
        </Button>
      </div>
      {editing && (
        <form
          className="mt-3 space-y-2 border-t border-dashed bg-surface-secondary px-3 py-3"
          onSubmit={(e) => {
            e.preventDefault();
            void actions.run(note.EditAction, { text });
          }}
        >
          <Field label="Edit note">
            <TextField label="Note text" value={text} onChange={setText} multiline rows={2} />
          </Field>
          <div className="flex justify-end">
            <Button size="sm" type="submit" isPending={actions.pending === note.EditAction} isDisabled={!text.trim() || text === note.Text}>
              Save
            </Button>
          </div>
        </form>
      )}
    </li>
  );
}
