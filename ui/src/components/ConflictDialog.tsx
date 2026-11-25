// The 412 dialog for config saves. Two variants: the file on disk is valid (compare and choose),
// or it is now invalid (show its errors, then replace it with a full PUT or cancel).
import { useId, useState } from "react";
import type { ConfigResponse } from "../gen/api";
import { strings } from "../strings";
import { settingsStrings } from "../views/Settings.strings";
import { Button, Dialog } from "./ui";

const t = settingsStrings.conflict;

export type ConflictChange = { path: string; label: string; mine: string; file: string };

export type ConflictDialogProps = {
  /** The 412 body: the file on disk now. */
  current: ConfigResponse;
  /** The fields you changed, with both values already formatted. */
  changes: ConflictChange[];
  /** Fields only the file changed; `file` holds their new value. */
  alsoChanged: ConflictChange[];
  busy: boolean;
  onKeepMine: () => void;
  onUseFile: () => void;
  onReview: () => void;
  onReplace: () => void;
  /** Closing the dialog: keep editing (valid file) or show the invalid-file view (invalid file). */
  onCancel: () => void;
};

export function ConflictDialog(p: ConflictDialogProps) {
  const [confirming, setConfirming] = useState(false);
  const alsoId = useId();
  const valid = p.current.state === "applied";
  return (
    <Dialog
      isOpen
      isDismissable={false}
      onOpenChange={(open) => !open && p.onCancel()}
      title={valid ? t.title : t.invalidTitle}
      size="lg"
    >
      {valid ? (
        <>
          <p>{t.body}</p>
          <ul className="conflict-changes">
            {p.changes.map((c) => (
              <li key={c.path}>
                <span className="conflict-field">{c.label}</span>
                <dl>
                  <dt>{t.yours}</dt>
                  <dd>{c.mine}</dd>
                  <dt>{t.inFile}</dt>
                  <dd>{c.file}</dd>
                </dl>
              </li>
            ))}
          </ul>
          {p.alsoChanged.length > 0 && (
            <>
              <h3 id={alsoId} className="conflict-also">
                {t.alsoChanged}
              </h3>
              <ul className="conflict-changes" aria-labelledby={alsoId}>
                {p.alsoChanged.map((c) => (
                  <li key={c.path}>
                    <span className="conflict-field">{c.label}</span>
                    <dl>
                      <dt>{t.inFile}</dt>
                      <dd>{c.file}</dd>
                    </dl>
                  </li>
                ))}
              </ul>
            </>
          )}
          <div className="conflict-actions">
            <Button onPress={p.onReview} isDisabled={p.busy}>
              {t.review}
            </Button>
            <Button onPress={p.onUseFile} isDisabled={p.busy}>
              {t.useFile}
            </Button>
            <Button variant="primary" onPress={p.onKeepMine} isPending={p.busy}>
              {t.keepMine}
            </Button>
          </div>
        </>
      ) : (
        <>
          <p>{t.invalidBody}</p>
          <ul className="conflict-errors">
            {p.current.errors.map((e) => (
              <li key={`${e.line}:${e.column}:${e.path}:${e.message}`}>
                {settingsStrings.errorAt(
                  settingsStrings.invalid.location(e.line, e.column),
                  e.message,
                )}
              </li>
            ))}
          </ul>
          {confirming && (
            <p className="conflict-warning" role="alert">
              {t.replaceWarning}
            </p>
          )}
          <div className="conflict-actions">
            {confirming ? (
              <>
                <Button onPress={() => setConfirming(false)} isDisabled={p.busy}>
                  {t.back}
                </Button>
                <Button variant="danger" onPress={p.onReplace} isPending={p.busy}>
                  {t.replaceConfirm}
                </Button>
              </>
            ) : (
              <>
                <Button onPress={p.onCancel}>{strings.buttons.cancel}</Button>
                <Button variant="primary" onPress={() => setConfirming(true)}>
                  {t.replace}
                </Button>
              </>
            )}
          </div>
        </>
      )}
    </Dialog>
  );
}
