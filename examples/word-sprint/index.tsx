import { useEffect, useMemo, useRef, useState } from 'react';
import { defineActivity } from 'bombers';
import { Button } from 'bombers/ui';

// Word Sprint — the first real game, and the proof that the whole stack works.
//
// Solo it's a plain typing test: type the words, get your speed. In a room it's
// a RACE — each player broadcasts their progress a few times a second and sees
// everyone else's bar move. That makes it the smallest thing that exercises the
// real path end to end: install, compile, the registry, a room, send, subscribe,
// presence, and the host/solo split.
//
// It is NOT bundled with the app. It lives here, gets published with
// `publish-game examples/word-sprint`, and installs from the store like anything
// else — which is the point. If the first game got special treatment, nothing
// would prove the ordinary path works.
//
// The relay stays dumb throughout. Nobody's score is refereed: this is a race
// among friends, and every number on screen is self-reported. Anything that
// needed to be unforgeable would have to be computed server-side, which is
// exactly the line drawn in ACTIVITIES.md.

const WORDS = `the quick brown fox jumps over a lazy dog while silent letters drift
through paper lanterns and someone counts backwards from ten in a kitchen that
smells of coffee rain taps the window glass a train hums past the garden wall
every keystroke is a small decision about where the sentence wants to go next`
  .split(/\s+/)
  .filter(Boolean);

const ROUND_WORDS = 30;
// How often a racer tells the room where they are. Ten times a second is far
// more than a typing race needs and still nowhere near the relay's budget — the
// cap is sized for 30Hz position streams.
const BROADCAST_MS = 100;

type Progress = { typed: number; wpm: number; accuracy: number; done: boolean };

function shuffled(seed: number): string[] {
  // A tiny deterministic shuffle so everyone in a room gets the same list from
  // the same seed without the server knowing what a word is.
  const out = [...WORDS];
  let s = seed || 1;
  for (let i = out.length - 1; i > 0; i--) {
    s = (s * 1664525 + 1013904223) % 4294967296;
    const j = s % (i + 1);
    [out[i], out[j]] = [out[j], out[i]];
  }
  return out.slice(0, ROUND_WORDS);
}

function WordSprint({ ctx }) {
  // One seed per playthrough id keeps a room's players on the same words without
  // a single byte of coordination.
  const seed = useMemo(() => {
    let h = 0;
    for (const ch of ctx.id) h = (h * 31 + ch.charCodeAt(0)) | 0;
    return Math.abs(h);
  }, [ctx.id]);
  const words = useMemo(() => shuffled(seed), [seed]);
  const target = useMemo(() => words.join(' '), [words]);

  const [typed, setTyped] = useState('');
  const [startedAt, setStartedAt] = useState<number | null>(null);
  const [finishedAt, setFinishedAt] = useState<number | null>(null);
  const [others, setOthers] = useState<Record<string, Progress>>({});
  const inputRef = useRef<HTMLInputElement>(null);

  const correct = countCorrect(typed, target);
  const elapsed = startedAt ? ((finishedAt ?? Date.now()) - startedAt) / 1000 : 0;
  const wpm = elapsed > 0 ? Math.round(correct / 5 / (elapsed / 60)) : 0;
  const accuracy = typed.length > 0 ? Math.round((correct / typed.length) * 100) : 100;
  const done = finishedAt !== null;

  // Listen for the others' progress. Subscribing is NOT React state, so a room
  // full of racers costs one re-render per update we choose to make, not one per
  // frame received.
  useEffect(() => {
    return ctx.subscribe((frame) => {
      if (frame.t !== 'sprint:progress' || !frame.from) return;
      const d = frame.d as Progress | null;
      if (!d) return;
      setOthers((prev) => ({ ...prev, [frame.from]: d }));
    });
  }, [ctx]);

  // Broadcast our own progress on a timer rather than per keystroke: a fast
  // typist would otherwise send 8-10 messages a second for no extra clarity.
  const progressRef = useRef<Progress>({ typed: 0, wpm: 0, accuracy: 100, done: false });
  progressRef.current = { typed: correct, wpm, accuracy, done };
  useEffect(() => {
    if (ctx.mode !== 'room') return;
    const t = window.setInterval(() => {
      ctx.send('sprint:progress', progressRef.current);
    }, BROADCAST_MS);
    return () => window.clearInterval(t);
  }, [ctx]);

  const onChange = (value: string) => {
    if (done) return;
    if (startedAt === null && value.length > 0) setStartedAt(Date.now());
    const next = value.slice(0, target.length);
    setTyped(next);
    if (next.length >= target.length) setFinishedAt(Date.now());
  };

  const reset = () => {
    setTyped('');
    setStartedAt(null);
    setFinishedAt(null);
    inputRef.current?.focus();
  };

  const roster = ctx.members.filter((m) => m.user_id !== ctx.youId);

  return (
    <div style={styles.root} onClick={() => inputRef.current?.focus()}>
      <div style={styles.stats}>
        <Stat label="WPM" value={String(wpm)} />
        <Stat label="Accuracy" value={`${accuracy}%`} />
        <Stat label="Progress" value={`${Math.min(typed.length, target.length)}/${target.length}`} />
        {done && (
          <Button size="sm" variant="secondary" onClick={reset}>
            Again
          </Button>
        )}
      </div>

      <div style={styles.passage}>
        {target.split('').map((ch, i) => {
          const typedCh = typed[i];
          const state = typedCh === undefined ? 'pending' : typedCh === ch ? 'right' : 'wrong';
          return (
            <span
              key={i}
              style={{
                ...styles.char,
                ...(state === 'right' ? styles.right : null),
                ...(state === 'wrong' ? styles.wrong : null),
                ...(i === typed.length ? styles.cursor : null),
              }}
            >
              {ch}
            </span>
          );
        })}
      </div>

      {/* The real input is invisible: the passage above IS the interface. */}
      <input
        ref={inputRef}
        autoFocus
        value={typed}
        onChange={(e) => onChange(e.target.value)}
        style={styles.hiddenInput}
        aria-label="Type the passage"
        spellCheck={false}
      />

      {ctx.mode === 'room' && (
        <div style={styles.race}>
          <div style={styles.raceLabel}>Race</div>
          <Racer name="You" progress={{ typed: correct, wpm, accuracy, done }} total={target.length} you />
          {roster.map((m) => (
            <Racer
              key={m.user_id}
              name={m.username || m.user_id.slice(0, 6)}
              progress={others[m.user_id] ?? { typed: 0, wpm: 0, accuracy: 100, done: false }}
              total={target.length}
            />
          ))}
          {roster.length === 0 && <div style={styles.waiting}>Waiting for someone to join this room…</div>}
        </div>
      )}
    </div>
  );
}

function Stat({ label, value }) {
  return (
    <span style={styles.stat}>
      <span style={styles.statValue}>{value}</span>
      <span style={styles.statLabel}>{label}</span>
    </span>
  );
}

function Racer({ name, progress, total, you }) {
  const pct = Math.min(100, Math.round((progress.typed / Math.max(total, 1)) * 100));
  return (
    <div style={styles.racer}>
      <span style={{ ...styles.racerName, ...(you ? { color: 'var(--text)', fontWeight: 600 } : null) }}>{name}</span>
      <span style={styles.track}>
        <span style={{ ...styles.fill, width: `${pct}%`, background: you ? 'var(--accent)' : 'var(--text-faint)' }} />
      </span>
      <span style={styles.racerWpm}>{progress.done ? 'done' : `${progress.wpm} wpm`}</span>
    </div>
  );
}

/** Characters typed that match the passage at the same position. */
function countCorrect(typed: string, target: string): number {
  let n = 0;
  for (let i = 0; i < typed.length && i < target.length; i++) {
    if (typed[i] === target[i]) n++;
  }
  return n;
}

export default defineActivity({
  id: 'word-sprint',
  name: 'Word Sprint',
  version: '1.0.0',
  description: 'A typing race. Alone against the clock, or side by side with friends.',
  category: 'Typing',
  players: { min: 1, max: 8 },
  modes: ['solo', 'room'],
  render: (ctx) => <WordSprint ctx={ctx} />,
});

// Everything is themed through the app's CSS variables, so a game inherits the
// user's colours, fonts and roundness for free — and changing theme restyles it
// without the game knowing.
const styles: Record<string, React.CSSProperties> = {
  root: {
    flex: 1,
    minWidth: 0,
    minHeight: 0,
    display: 'flex',
    flexDirection: 'column',
    gap: 18,
    padding: '28px 32px',
    overflowY: 'auto',
    cursor: 'text',
  },
  stats: { display: 'flex', alignItems: 'flex-end', gap: 24 },
  stat: { display: 'flex', flexDirection: 'column' },
  statValue: { fontSize: 24, fontWeight: 700, color: 'var(--text)', lineHeight: 1.1 },
  statLabel: { fontSize: 11, letterSpacing: '0.06em', textTransform: 'uppercase', color: 'var(--text-faint)' },
  passage: {
    fontSize: 20,
    lineHeight: 1.7,
    fontFamily: 'var(--font-mono, monospace)',
    color: 'var(--text-faint)',
    maxWidth: 820,
    userSelect: 'none',
  },
  char: { whiteSpace: 'pre-wrap' },
  right: { color: 'var(--text)' },
  wrong: { color: 'var(--danger)', textDecoration: 'underline' },
  cursor: { borderLeft: '2px solid var(--accent)', marginLeft: -2 },
  hiddenInput: { position: 'absolute', opacity: 0, width: 1, height: 1, pointerEvents: 'none' },
  race: { display: 'flex', flexDirection: 'column', gap: 6, maxWidth: 520, marginTop: 4 },
  raceLabel: {
    fontSize: 10.5,
    letterSpacing: '0.06em',
    textTransform: 'uppercase',
    fontWeight: 700,
    color: 'var(--text-faint)',
  },
  racer: { display: 'flex', alignItems: 'center', gap: 10 },
  racerName: {
    width: 100,
    fontSize: 12.5,
    color: 'var(--text-muted)',
    overflow: 'hidden',
    textOverflow: 'ellipsis',
    whiteSpace: 'nowrap',
  },
  track: { flex: 1, height: 6, borderRadius: 3, background: 'var(--surface-2)', overflow: 'hidden' },
  fill: { display: 'block', height: '100%', transition: 'width 120ms linear' },
  racerWpm: { width: 58, textAlign: 'right', fontSize: 11.5, color: 'var(--text-faint)' },
  waiting: { fontSize: 12, color: 'var(--text-faint)' },
};
