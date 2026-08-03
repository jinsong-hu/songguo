import { afterEach, describe, expect, it } from 'vitest';
import { cleanup, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useState } from 'react';
import { FacetSelect } from './FacetSelect';
import type { Facet } from '../api/types';

// Vitest runs with globals:false, so Testing Library cannot register its own
// afterEach — without this, each test's DOM leaks into the next and queries
// match elements from a previous render.
afterEach(cleanup);

const OPTIONS: Facet[] = [
  { key: 'claude-opus-5', requests: 1200 },
  { key: 'claude-haiku-4-5', requests: 340 },
  { key: 'gpt-5.6-sol', requests: 12 },
];

/** Wrapper that holds state, so the control is exercised the way the page uses it. */
function Harness({
  options = OPTIONS,
  initial = [],
  onChange,
  renderLabel,
}: {
  options?: Facet[];
  initial?: string[];
  onChange?: (next: string[]) => void;
  renderLabel?: (key: string) => string;
}) {
  const [value, setValue] = useState<string[]>(initial);
  return (
    <FacetSelect
      label="models"
      options={options}
      value={value}
      onChange={(next) => {
        setValue(next);
        onChange?.(next);
      }}
      renderLabel={renderLabel}
    />
  );
}

// The trigger and the panel's reset button can both read "All models", so pick
// the trigger by the one attribute only it carries rather than by name.
const trigger = () =>
  screen
    .getAllByRole('button')
    .find((b) => b.getAttribute('aria-haspopup') === 'dialog') as HTMLButtonElement;

const open = async (user: ReturnType<typeof userEvent.setup>) => {
  await user.click(trigger());
  return screen.getByRole('dialog', { name: 'models' });
};

const checkbox = (name: RegExp) => screen.getByRole('checkbox', { name }) as HTMLInputElement;

describe('FacetSelect', () => {
  it('defaults to all and opens on click', async () => {
    const user = userEvent.setup();
    render(<Harness />);

    expect(trigger().textContent).toContain('All models');
    expect(screen.queryByRole('dialog')).toBeNull();

    await open(user);
    expect(screen.getByRole('dialog', { name: 'models' })).toBeDefined();
    // Every option is listed, none checked.
    for (const o of OPTIONS) {
      expect(checkbox(new RegExp(escape(o.key))).checked).toBe(false);
    }
  });

  it('selecting one reports it and labels the trigger with its name', async () => {
    const user = userEvent.setup();
    const seen: string[][] = [];
    render(<Harness onChange={(n) => seen.push(n)} />);

    await open(user);
    await user.click(checkbox(/claude-opus-5/));

    expect(seen).toEqual([['claude-opus-5']]);
    expect(trigger().textContent).toContain('claude-opus-5');
  });

  it('stays open across toggles and counts a multi-selection', async () => {
    const user = userEvent.setup();
    render(<Harness />);

    await open(user);
    await user.click(checkbox(/claude-opus-5/));
    // The panel must survive the first pick — closing after each would make
    // choosing three models three round trips.
    expect(screen.getByRole('dialog', { name: 'models' })).toBeDefined();
    await user.click(checkbox(/claude-haiku-4-5/));

    expect(trigger().textContent).toContain('2 models');
  });

  it('unchecking the last selection returns to All, never to an empty result', async () => {
    const user = userEvent.setup();
    render(<Harness initial={['claude-opus-5']} />);

    await open(user);
    await user.click(checkbox(/claude-opus-5/));

    expect(trigger().textContent).toContain('All models');
  });

  it('the reset button clears the selection and then reads as already-the-case', async () => {
    const user = userEvent.setup();
    render(<Harness initial={['claude-opus-5', 'gpt-5.6-sol']} />);

    const dialog = await open(user);
    const reset = within(dialog).getByRole('button', { name: 'All models' }) as HTMLButtonElement;
    expect(reset.disabled).toBe(false);

    await user.click(reset);

    expect(trigger().textContent).toContain('All models');
    expect(reset.disabled).toBe(true);
  });

  it('keeps a selection that has dropped out of the options, so it can still be cleared', async () => {
    const user = userEvent.setup();
    // Narrowing the time range can drop a model out from under an active
    // selection. It must still render — a filter you cannot see is one you
    // cannot clear.
    render(<Harness options={[OPTIONS[0]]} initial={['gpt-5.6-sol']} />);

    await open(user);
    expect(checkbox(/gpt-5\.6-sol/).checked).toBe(true);
  });

  it('Escape closes the panel without changing the selection', async () => {
    const user = userEvent.setup();
    const seen: string[][] = [];
    render(<Harness onChange={(n) => seen.push(n)} />);

    await open(user);
    await user.keyboard('{Escape}');

    expect(screen.queryByRole('dialog')).toBeNull();
    expect(seen).toEqual([]);
  });

  it('hides the search box for a short list', async () => {
    const user = userEvent.setup();
    render(<Harness />);

    await open(user);
    expect(screen.queryByRole('textbox', { name: /Filter models/ })).toBeNull();
  });

  it('filters the list by the search box once the list is long', async () => {
    const user = userEvent.setup();
    const many: Facet[] = Array.from({ length: 10 }, (_, i) => ({
      key: i === 0 ? 'claude-opus-5' : `model-${i}`,
      requests: 10 - i,
    }));
    render(<Harness options={many} />);

    await open(user);
    await user.type(screen.getByRole('textbox', { name: /Filter models/ }), 'opus');

    expect(checkbox(/claude-opus-5/)).toBeDefined();
    expect(screen.queryByRole('checkbox', { name: /model-4/ })).toBeNull();
  });

  it('searches on the display name but still reports the raw key', async () => {
    const user = userEvent.setup();
    // The Clients filter renders "Claude Code" over the key `claude-code`.
    // Searching has to match what the user can see, while the selection stays
    // the ledger value the API filters on.
    const seen: string[][] = [];
    const many: Facet[] = Array.from({ length: 10 }, (_, i) => ({
      key: i === 0 ? 'claude-code' : `model-${i}`,
      requests: 10 - i,
    }));
    render(
      <Harness
        options={many}
        onChange={(n) => seen.push(n)}
        renderLabel={(k) => (k === 'claude-code' ? 'Claude Code' : k)}
      />,
    );

    await open(user);
    await user.type(screen.getByRole('textbox', { name: /Filter models/ }), 'Claude Co');

    expect(screen.queryByRole('checkbox', { name: /model-4/ })).toBeNull();

    await user.click(checkbox(/Claude Code/));
    expect(seen).toEqual([['claude-code']]);
    // A lone selection labels the trigger with its display name, not its key.
    expect(trigger().textContent).toContain('Claude Code');
  });
});

/** Escape regex metacharacters in a model id (e.g. the dot in gpt-5.6-sol). */
function escape(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}
