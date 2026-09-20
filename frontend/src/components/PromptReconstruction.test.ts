import { describe, expect, it } from 'vitest';
import { parsePromptReconstruction } from './PromptReconstruction';
import type { SessionMessages } from '../api/types';

function view(overrides: Partial<SessionMessages> = {}): SessionMessages {
  return { session_id: '', model: 'gpt-5', system: [], tools: [], messages: [], reply: [], ...overrides };
}

describe('parsePromptReconstruction reply', () => {
  it('reads a Responses reply as an assistant turn plus its reasoning', () => {
    const prompt = parsePromptReconstruction(
      view({
        reply: [
          { type: 'reasoning', id: 'rs_1', summary: [], content: [{ type: 'reasoning_text', text: 'weighing it' }] },
          { type: 'message', id: 'msg_1', role: 'assistant', content: [{ type: 'output_text', text: '42' }] },
        ],
      }),
    );

    expect(prompt.reply.map((message) => message.role)).toEqual(['assistant', 'assistant']);
    // A bare reasoning item declares no role; labelling the card "reasoning"
    // would read as a speaker that does not exist.
    expect(prompt.reply[0].parts).toEqual([
      expect.objectContaining({ kind: 'raw', label: 'reasoning' }),
    ]);
    expect(prompt.reply[1].parts).toEqual([{ kind: 'text', text: '42' }]);
  });

  it('reads an Anthropic reply, thinking block and all', () => {
    const prompt = parsePromptReconstruction(
      view({
        reply: [
          {
            role: 'assistant',
            content: [
              { type: 'thinking', thinking: 'weighing it', signature: 'sig' },
              { type: 'text', text: '42' },
            ],
          },
        ],
      }),
    );

    expect(prompt.reply).toHaveLength(1);
    expect(prompt.reply[0].role).toBe('assistant');
    expect(prompt.reply[0].parts[0]).toEqual(expect.objectContaining({ kind: 'raw', label: 'thinking' }));
    expect(prompt.reply[0].parts[1]).toEqual({ kind: 'text', text: '42' });
  });

  it('keeps the reply out of the context blocks', () => {
    const prompt = parsePromptReconstruction(
      view({
        messages: [{ role: 'user', content: 'what is six times seven' }],
        reply: [{ role: 'assistant', content: [{ type: 'text', text: 'x'.repeat(4000) }] }],
      }),
    );

    // The blocks feed the context sunburst and the token estimates, which
    // describe the INPUT window. A 4,000-character reply counted there would
    // inflate every context number on the page.
    expect(prompt.blocks).toHaveLength(1);
    expect(prompt.blocks[0].source).toBe('user');
  });

  it('has an empty reply when the backend sent none', () => {
    expect(parsePromptReconstruction(view()).reply).toEqual([]);
  });
});

// The reasoning text probe is read through a prompt-side block, whose snippet
// is exactly what the panel renders for the same part.
describe('reasoning text', () => {
  // A Responses reasoning item routinely arrives with an empty summary and its
  // whole text in content[]; stopping at the summary renders it blank.
  it('falls back from summary to content', () => {
    const fromContent = parsePromptReconstruction(
      view({ messages: [{ type: 'reasoning', summary: [], content: [{ type: 'reasoning_text', text: 'in content' }] }] }),
    );
    expect(fromContent.blocks.map((block) => block.snippet)).toEqual(['in content']);

    const fromSummary = parsePromptReconstruction(
      view({
        messages: [
          {
            type: 'reasoning',
            summary: [{ type: 'summary_text', text: 'in summary' }],
            content: [{ type: 'reasoning_text', text: 'in content' }],
          },
        ],
      }),
    );
    expect(fromSummary.blocks.map((block) => block.snippet)).toEqual(['in summary']);
  });

  // Reasoning weighs its text only. A signature, and encrypted_content, are
  // opaque bytes and are never rendered — the trace panel is one click away.
  it('weighs a thinking block by its text, not its envelope', () => {
    const prompt = parsePromptReconstruction(
      view({
        messages: [
          {
            role: 'assistant',
            content: [{ type: 'thinking', thinking: 'weighing it', signature: 'x'.repeat(4000) }],
          },
        ],
      }),
    );
    expect(prompt.blocks).toHaveLength(1);
    expect(prompt.blocks[0].producer).toBe('reasoning');
    expect(prompt.blocks[0].snippet).toBe('weighing it');
  });
});
