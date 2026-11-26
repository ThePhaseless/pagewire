// Consent to one vendor licence (catalog EULA). The text comes from GET /eulas/{id} and the `eula`
// SSE event: the device agent reads it from the verified vendor archive. Accepting records the
// sha256 of that text.
import { useContext, useEffect, useState } from "react";
import { api, problemMessage } from "../api/client";
import { EventsContext } from "../api/events";
import type { EULA, EULARef, SetupEULA } from "../gen/api";
import { strings } from "../strings";
import { eulaStrings as t } from "./EulaAccept.strings";
import { Button, Checkbox, InlineError, ProgressBar } from "./ui";
import "./EulaAccept.css";

/** The textSha256 accepted for id@version in `list`, or undefined when it is not accepted. */
export function acceptedSha(list: EULARef[], id: string, version: string): string | undefined {
  const r = list.find((r) => r.id === id && r.version === version);
  return r ? (r.textSha256 ?? "") : undefined;
}

/** `list` with id@version accepted for `textSha256`, or removed when it is null. */
export function withAccepted(
  list: EULARef[],
  id: string,
  version: string,
  textSha256: string | null,
): EULARef[] {
  const rest = list.filter((r) => !(r.id === id && r.version === version));
  return textSha256 ? [...rest, { id, version, textSha256 }] : rest;
}

/**
 * GET /eulas/{id}, kept current by the `eula` SSE events for that id (the agent's retry shows up
 * without a reload), and asked again when the stream reconnects.
 */
function useEula(id: string) {
  const store = useContext(EventsContext);
  const [eula, setEula] = useState<EULA | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [attempt, setAttempt] = useState(0);
  // biome-ignore lint/correctness/useExhaustiveDependencies: attempt is a refetch trigger
  useEffect(() => {
    let live = true;
    // Counts events; a GET answer is dropped when an event arrived after the GET started.
    let events = 0;
    // Listen before the GET, so an event sent while it is in flight is not missed.
    const offEvent = store.on("eula", (e) => {
      if (e.id !== id) return;
      events++;
      setEula(e);
      setError(null);
    });
    const load = () => {
      const seen = events;
      api.getEula(id).then(
        (e) => {
          if (!live || seen !== events) return;
          setEula(e);
          setError(null);
        },
        (err) => {
          if (live && seen === events) setError(err);
        },
      );
    };
    load();
    const offReconnect = store.onReconnect(load);
    return () => {
      live = false;
      offEvent();
      offReconnect();
    };
  }, [store, id, attempt]);
  return { eula, error, retry: () => setAttempt((n) => n + 1) };
}

export function EulaAccept({
  eula: info,
  accepted,
  onChange,
}: {
  eula: SetupEULA;
  /** The textSha256 recorded for this licence (acceptedSha), or undefined when not accepted. */
  accepted: string | undefined;
  /** The sha256 of the text shown when ticked, null when unticked. */
  onChange: (textSha256: string | null) => void;
}) {
  const { eula, error, retry } = useEula(info.id);
  const current = eula?.textSha256 ?? null;
  // Only a text the agent has read and that differs counts as changed; pending is not.
  const changed = accepted !== undefined && current !== null && current !== accepted;
  const isAccepted = accepted !== undefined && !changed;
  const available = eula?.state === "available" && current !== null;

  return (
    <div className="ui-field eula">
      <h3>{info.title}</h3>
      <p className="ui-field-description">
        {t.meta(info.vendor, info.version)} · {info.required ? t.required : t.optional}
      </p>
      <p>{info.summary}</p>
      {error ? (
        <InlineError action={<Button onPress={retry}>{strings.buttons.retry}</Button>}>
          <p>{t.loadFailed}</p>
          <p>{problemMessage(error)}</p>
        </InlineError>
      ) : !eula ? (
        <p role="status">{t.loading}</p>
      ) : eula.state === "pending" ? (
        <ProgressBar label={t.pending(info.vendor)} />
      ) : eula.state === "failed" ? (
        <InlineError action={<Button onPress={retry}>{strings.buttons.retry}</Button>}>
          <p>{t.failed(info.title)}</p>
          {eula.message && <p>{eula.message}</p>}
        </InlineError>
      ) : (
        // A focusable region, so keyboard users can scroll the text.
        // biome-ignore lint/a11y/noNoninteractiveTabindex: scrollable content must be reachable by keyboard
        <section className="eula-text" aria-label={t.textLabel(info.title)} tabIndex={0}>
          <pre>{eula.text}</pre>
        </section>
      )}
      {changed && (
        <p className="eula-changed" role="status">
          {t.changed}
        </p>
      )}
      <Checkbox
        isSelected={isAccepted}
        // Revoking always works; accepting needs the text on screen.
        isDisabled={!isAccepted && !available}
        onChange={(on) => onChange(on ? current : null)}
      >
        {t.accept(info.title, info.version)}
      </Checkbox>
      <p className="ui-field-description">{t.howItWorks(info.vendor)}</p>
    </div>
  );
}
