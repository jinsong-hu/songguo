import { useRef, useState } from 'react';
import { ChevronRight } from 'lucide-react';
import type { SessionMessages } from '../api/types';
import { CopyButton } from './CopyButton';
import { ErrorBanner } from './ErrorBanner';
import { Skeleton } from './Skeleton';
import styles from '../pages/Detail.module.css';

// The System Prompt / Tools / Messages panels, rendered from the backend's
// prompt view (GET /api/sessions/{id}/messages or /api/calls/{id}/messages).
// Shared by the session page, which merges every request, and the request page,
// which shows the one prompt that call sent.
//
// showResponse adds the Response panel, and only the request page sets it. The
// backend fills `reply` for a single call alone — a session merges requests,
// each of which already carries the previous turn's reply in its history — so
// on a session the panel could only ever render empty.
export function PromptReconstructionCard({
  prompt,
  loading,
  error,
  onRetry,
  showResponse = false,
}: {
  prompt: PromptReconstruction | null;
  loading: boolean;
  error?: string | null;
  onRetry: () => void;
  showResponse?: boolean;
}) {
  if (error) return <ErrorBanner message={error} onRetry={onRetry} />;

  if (loading || !prompt) {
    return (
      <div className={styles.promptGrid}>
        <Skeleton height={180} />
        <Skeleton height={180} />
        <Skeleton height={220} />
        {showResponse ? <Skeleton height={180} /> : null}
      </div>
    );
  }

  return (
    <div className={styles.promptGrid}>
      <details className={styles.promptPanel}>
        <summary className={styles.promptPanelHead}>
          <span className={styles.promptPanelTitle}>
            <ChevronRight size={15} className={styles.promptChevron} />
            System Prompt
          </span>
          <span className="chip chip-mono">{prompt.system.length}</span>
        </summary>
        <div className={styles.promptScroll}>
          {prompt.system.length > 0 ? (
            prompt.system.map((block, i) => (
              <pre key={i} id={systemBlockDomId(i)} className={styles.promptTextBlock}>{block}</pre>
            ))
          ) : (
            <div className={styles.promptEmpty}>No system content in this request.</div>
          )}
        </div>
      </details>

      <details className={styles.promptPanel}>
        <summary className={styles.promptPanelHead}>
          <span className={styles.promptPanelTitle}>
            <ChevronRight size={15} className={styles.promptChevron} />
            Tools
          </span>
          <span className="chip chip-mono">{prompt.tools.length}</span>
        </summary>
        <div className={styles.toolList}>
          {prompt.tools.length > 0 ? (
            prompt.tools.map((tool, i) => (
              <ToolCard key={`${tool.name}-${i}`} tool={tool} index={i} />
            ))
          ) : (
            <div className={styles.promptEmpty}>No tools in this request.</div>
          )}
        </div>
      </details>

      <details className={styles.promptPanel}>
        <summary className={styles.promptPanelHead}>
          <span className={styles.promptPanelTitle}>
            <ChevronRight size={15} className={styles.promptChevron} />
            Messages
          </span>
          <span className="chip chip-mono">{prompt.messages.length}</span>
        </summary>
        {prompt.omittedRequests > 0 ? (
          <div className={styles.promptEmpty}>
            Starts part-way through: the {prompt.omittedRequests} oldest{' '}
            {prompt.omittedRequests === 1 ? 'request was' : 'requests were'} too large to read with the rest.
          </div>
        ) : null}
        {prompt.messages.length > 0 ? (
          <div className={styles.messageTimeline}>
            {prompt.messages.map((message, i) => (
              <MessageCard key={`${message.role}-${i}`} message={message} index={i} />
            ))}
          </div>
        ) : (
          <div className={styles.promptEmpty}>No messages captured.</div>
        )}
      </details>

      {showResponse ? (
        <details className={styles.promptPanel}>
          <summary className={styles.promptPanelHead}>
            <span className={styles.promptPanelTitle}>
              <ChevronRight size={15} className={styles.promptChevron} />
              Response
            </span>
            <span className="chip chip-mono">{prompt.reply.length}</span>
          </summary>
          {prompt.reply.length > 0 ? (
            <div className={styles.messageTimeline}>
              {prompt.reply.map((message, i) => (
                <MessageCard key={`${message.role}-${i}`} message={message} index={i} scope="reply" />
              ))}
            </div>
          ) : (
            <div className={styles.promptEmpty}>No response captured for this call.</div>
          )}
        </details>
      ) : null}
    </div>
  );
}

function MessageCard({
  message,
  index,
  scope = 'prompt',
}: {
  message: PromptMessage;
  index: number;
  // scope namespaces the part DOM ids. The reply renders the same components as
  // the prompt on the same page, so without it both panels would mint
  // "prompt-message-0-part-0" and every jump would land on whichever came first.
  scope?: string;
}) {
  return (
    <div className={styles.messageCard}>
      <div className={styles.messageRole}>{message.role}</div>
      <div className={styles.messageContent}>
        {message.parts.map((part, i) => (
          <MessagePartView
            key={i}
            part={part}
            domId={messagePartDomId(index, [i], scope)}
            path={[i]}
            messageIndex={index}
            scope={scope}
          />
        ))}
      </div>
    </div>
  );
}

function MessagePartView({
  part,
  domId,
  messageIndex,
  path,
  scope = 'prompt',
}: {
  part: MessagePart;
  domId: string;
  messageIndex: number;
  path: number[];
  scope?: string;
}) {
  if (part.kind === 'text') {
    return (
      <div id={domId} className={`${styles.messagePartBlock} ${isSingleLineText(part.text) ? styles.messagePartSingleLine : ''}`}>
        <CopyButton value={messagePartCopyText(part)} ariaLabel="Copy text block" className={styles.messagePartCopy} />
        <pre className={styles.messageText}>{part.text}</pre>
      </div>
    );
  }
  if (part.kind === 'tool_use') {
    return (
      <div id={part.id ? toolPartDomId('use', part.id) : domId} className={styles.messageToolBlock}>
        <div className={styles.messageToolHead}>
          <span className={styles.messageToolTitle}>
            <span>Tool Use</span>
            {part.name ? <span className={styles.messageToolName}>{part.name}</span> : null}
          </span>
          {part.id ? (
            <button
              type="button"
              className={styles.messageToolId}
              onClick={() => jumpToToolPart('result', part.id)}
              title="Jump to tool result"
            >
              {part.id}
            </button>
          ) : null}
        </div>
        {part.input !== undefined ? (
          <div className={styles.messageToolContent}>
            <div className={styles.messagePartBlock}>
              <CopyButton value={prettyJson(part.input)} ariaLabel="Copy tool input" className={styles.messagePartCopy} />
              <pre className={styles.messageJson}>{prettyJson(part.input)}</pre>
            </div>
          </div>
        ) : null}
      </div>
    );
  }
  if (part.kind === 'tool_result') {
    return (
      <div id={part.toolUseId ? toolPartDomId('result', part.toolUseId) : domId} className={styles.messageToolBlock}>
        <div className={styles.messageToolHead}>
          <span className={styles.messageToolTitle}>
            <span>Tool Result</span>
          </span>
          {part.toolUseId ? (
            <button
              type="button"
              className={styles.messageToolId}
              onClick={() => jumpToToolPart('use', part.toolUseId)}
              title="Jump to tool use"
            >
              {part.toolUseId}
            </button>
          ) : null}
        </div>
        {part.parts.length > 0 ? (
          <div className={styles.messageToolContent}>
            {part.parts.map((child, i) => (
              <MessagePartView
                key={i}
                part={child}
                domId={messagePartDomId(messageIndex, [...path, i], scope)}
                messageIndex={messageIndex}
                path={[...path, i]}
                scope={scope}
              />
            ))}
          </div>
        ) : null}
      </div>
    );
  }
  if (part.kind === 'image') {
    return (
      <figure id={domId} className={styles.messageImageBlock}>
        <CopyButton value={messagePartCopyText(part)} ariaLabel="Copy image source" className={styles.messagePartCopy} />
        <img className={styles.messageImage} src={part.src} alt={part.label} loading="lazy" />
        <figcaption className={styles.messageImageCaption}>{part.label}</figcaption>
      </figure>
    );
  }
  if (part.kind === 'image_url') {
    return (
      <figure id={domId} className={styles.messageImageBlock}>
        <CopyButton value={messagePartCopyText(part)} ariaLabel="Copy image URL" className={styles.messagePartCopy} />
        <img className={styles.messageImage} src={part.url} alt={part.label} loading="lazy" />
        <figcaption className={styles.messageImageCaption}>{part.label}</figcaption>
      </figure>
    );
  }
  if (part.kind === 'empty') {
    return (
      <div id={domId} className={styles.messageEmptyPart}>
        <span>{part.label}</span>
        <CopyButton value={messagePartCopyText(part)} ariaLabel="Copy block" className={styles.messagePartCopy} />
      </div>
    );
  }
  if (part.kind === 'raw' && isReasoningLabel(part.label)) {
    // Reasoning is usually an order of magnitude longer than the answer it
    // precedes, so it opens closed: expanded by default it buries the reply.
    // Only the text is rendered — an encrypted_content or signature field is
    // opaque bytes, and is one click away in the trace panel for anyone who
    // wants it.
    const text = reasoningPartText(part);
    return (
      <div id={domId} className={styles.messageToolBlock}>
        <details className={styles.messageReasoning}>
          <summary className={`${styles.messageToolHead} ${styles.messageReasoningHead}`}>
            <span className={styles.messageToolTitle}>
              <ChevronRight size={14} className={styles.messageReasoningChevron} />
              <span>Thinking</span>
              {part.label !== 'thinking' ? <span className={styles.messageToolName}>{part.label}</span> : null}
            </span>
          </summary>
          <div className={styles.messageToolContent}>
            {text ? (
              <div className={styles.messagePartBlock}>
                <CopyButton value={text} ariaLabel="Copy thinking" className={styles.messagePartCopy} />
                <pre className={styles.messageText}>{text}</pre>
              </div>
            ) : (
              <div className={styles.promptEmpty}>No readable thinking text in this block.</div>
            )}
          </div>
        </details>
      </div>
    );
  }
  return (
    <div id={domId} className={`${styles.messagePartBlock} ${styles.messagePartSingleLine}`}>
      <CopyButton value={messagePartCopyText(part)} ariaLabel="Copy raw block" className={styles.messagePartCopy} />
      <details className={styles.messageRawPart}>
        <summary>{part.label}</summary>
        <pre className={styles.messageJson}>{prettyJson(part.raw)}</pre>
      </details>
    </div>
  );
}

function ToolCard({ tool, index }: { tool: ToolInfo; index: number }) {
  const ref = useRef<HTMLDetailsElement | null>(null);
  const [align, setAlign] = useState<'left' | 'right'>('left');

  function handleToggle() {
    const el = ref.current;
    if (!el?.open) return;
    const rect = el.getBoundingClientRect();
    const viewportWidth = window.innerWidth;
    const panelWidth = Math.min(760, viewportWidth - 80);
    const margin = 24;
    const overflowsRight = rect.left + panelWidth > viewportWidth - margin;
    const fitsLeft = rect.right - panelWidth >= margin;
    setAlign(overflowsRight && fitsLeft ? 'right' : 'left');
  }

  return (
    <details ref={ref} id={toolBlockDomId(index)} className={styles.toolCard} onToggle={handleToggle}>
      <summary className={styles.toolSummary}>
        <span className={styles.toolName}>{tool.name || '(unnamed tool)'}</span>
        <span className={styles.toolMeta}>{toolMeta(tool)}</span>
      </summary>
      <div className={`${styles.toolDetail} ${align === 'right' ? styles.toolDetailRight : styles.toolDetailLeft}`}>
        {tool.description ? <p className={styles.toolDesc}>{tool.description}</p> : null}
        {tool.type === 'namespace' ? (
          <div className={styles.namespaceToolList}>
            {tool.tools.length > 0 ? (
              tool.tools.map((child, i) => (
                <NestedToolCard key={`${child.name}-${i}`} tool={child} />
              ))
            ) : (
              <div className={styles.schemaEmpty}>No tools in this namespace.</div>
            )}
          </div>
        ) : (
          <SchemaView schema={tool.schema} />
        )}
      </div>
    </details>
  );
}

function NestedToolCard({ tool }: { tool: ToolInfo }) {
  if (tool.type === 'namespace') {
    return (
      <details className={styles.namespaceNestedCard}>
        <summary className={styles.namespaceNestedSummary}>
          <span className={styles.toolName}>{tool.name || '(unnamed namespace)'}</span>
          <span className={styles.toolMeta}>{toolMeta(tool)}</span>
        </summary>
        {tool.description ? <p className={styles.toolDesc}>{tool.description}</p> : null}
        <div className={styles.namespaceToolList}>
          {tool.tools.length > 0 ? (
            tool.tools.map((child, i) => (
              <NestedToolCard key={`${child.name}-${i}`} tool={child} />
            ))
          ) : (
            <div className={styles.schemaEmpty}>No tools in this namespace.</div>
          )}
        </div>
      </details>
    );
  }

  return (
    <details className={styles.namespaceNestedCard}>
      <summary className={styles.namespaceNestedSummary}>
        <span className={styles.toolName}>{tool.name || '(unnamed tool)'}</span>
        <span className={styles.toolMeta}>{toolMeta(tool)}</span>
      </summary>
      {tool.description ? <p className={styles.toolDesc}>{tool.description}</p> : null}
      <SchemaView schema={tool.schema} />
    </details>
  );
}

function toolMeta(tool: ToolInfo): string {
  if (tool.type === 'namespace') return `${tool.tools.length} ${tool.tools.length === 1 ? 'tool' : 'tools'}`;
  return `${tool.propertyCount} ${tool.propertyCount === 1 ? 'input' : 'inputs'}`;
}

export interface PromptReconstruction {
  model: string;
  system: string[];
  tools: ToolInfo[];
  messages: PromptMessage[];
  /**
   * The assistant turn decoded from the call's captured response. Deliberately
   * absent from `blocks`: those describe the INPUT window that fed the context
   * sunburst and the token estimates, and a reply counted there would inflate
   * every context number on the page with tokens the request never carried.
   * Always empty on the session page.
   */
  reply: PromptMessage[];
  blocks: PromptBlock[];
  /** Oldest request bodies left unread over the backend's read budget. */
  omittedRequests: number;
}

interface ToolInfo {
  type: string;
  name: string;
  description: string;
  propertyCount: number;
  schema: unknown;
  tools: ToolInfo[];
  hashText: string;
}

export interface PromptBlock {
  id: string;
  source: string;
  producer?: string;
  tokens: number;
  hash: string;
  title: string;
  detail: string;
  snippet: string;
}

interface PromptMessage {
  role: string;
  parts: MessagePart[];
}

type MessagePart =
  | { kind: 'text'; text: string }
  | { kind: 'tool_use'; id: string; name: string; input: unknown; hashLabel?: string; hashText?: string }
  | { kind: 'tool_result'; toolUseId: string; parts: MessagePart[] }
  | { kind: 'image'; src: string; label: string; detail?: string; width?: number; height?: number; visualTokens?: number; hashLabel?: string; hashText?: string }
  | { kind: 'image_url'; url: string; label: string; detail?: string; width?: number; height?: number; visualTokens?: number; hashLabel?: string; hashText?: string }
  | { kind: 'empty'; label: string }
  | { kind: 'raw'; label: string; raw: unknown };

export function parsePromptReconstruction(source: SessionMessages): PromptReconstruction {
  let system: string[] = [];
  for (const value of source.system) {
    system = mergeUnique(system, systemBlocks(value), (block) =>
      compositionBlockHash('system', '', 'System prompt', block),
    );
  }
  const tools = mergeUnique([] as ToolInfo[], toolInfos(source.tools), (tool) =>
    compositionBlockHash('tool_schemas', tool.name || 'unknown', tool.name || 'Tool schema', tool.hashText),
  );
  const messages = source.messages.map(promptMessage).filter((message) => message.parts.length > 0);
  // The reply goes through the same renderer as the prompt because the backend
  // hands it back in the same item shape — that is the whole point of decoding
  // it wire-side rather than here.
  const reply = (source.reply ?? []).map(promptMessage).filter((message) => message.parts.length > 0);

  const prompt = {
    model: source.model,
    system,
    tools,
    messages,
    omittedRequests: source.omitted_requests ?? 0,
  };
  return {
    ...prompt,
    reply,
    blocks: promptBlocks(prompt),
  };
}

function mergeUnique<T>(existing: T[], next: T[], keyOf: (item: T) => string): T[] {
  if (next.length === 0) return existing;
  const seen = new Set(existing.map(keyOf));
  const merged = existing.slice();
  for (const item of next) {
    const key = keyOf(item);
    if (seen.has(key)) continue;
    seen.add(key);
    merged.push(item);
  }
  return merged;
}

// promptBlocks weighs the INPUT window only, so `reply` is excluded at the type
// level rather than by remembering not to read it.
function promptBlocks(prompt: Omit<PromptReconstruction, 'blocks' | 'reply'>): PromptBlock[] {
  const out: PromptBlock[] = [];

  prompt.system.forEach((block, i) => {
    const title = `System block ${i + 1}`;
    const detail = 'System prompt';
    out.push({
      id: systemBlockDomId(i),
      source: 'system',
      producer: 'base',
      tokens: estimateTokens(block),
      hash: compositionBlockHash('system', 'base', 'System prompt', block),
      title,
      detail,
      snippet: snippet(block),
    });
  });

  prompt.tools.forEach((tool, i) => {
    const text = toolBlockText(tool);
    const producer = tool.name || 'unknown';
    const title = tool.name || `Tool schema ${i + 1}`;
    const detail = 'Tool schema';
    out.push({
      id: toolBlockDomId(i),
      source: 'tool_schemas',
      producer,
      tokens: estimateTokens(text),
      hash: compositionBlockHash('tool_schemas', producer, title, tool.hashText),
      title,
      detail,
      snippet: tool.description || toolMeta(tool),
    });
  });

  const toolNames = toolUseNames(prompt.messages);
  prompt.messages.forEach((message, messageIndex) => {
    message.parts.forEach((part, partIndex) => {
      collectPartBlocks(out, message.role, part, messageIndex, [partIndex], toolNames, prompt.model);
    });
  });

  return out;
}

function collectPartBlocks(
  out: PromptBlock[],
  role: string,
  part: MessagePart,
  messageIndex: number,
  path: number[],
  toolNames: Map<string, string>,
  model: string,
) {
  const baseId = messagePartDomId(messageIndex, path);
  const baseDetail = `${role} message`;
  if (part.kind === 'tool_result') {
    // Producer is the request's verbatim tool name, mirroring the backend.
    const producer = toolNames.get(part.toolUseId) || 'unknown';
    const text = part.parts.map(messagePartSnippetText).join('\n');
    const title = part.toolUseId ? `Tool result ${part.toolUseId}` : 'Tool result';
    out.push({
      id: part.toolUseId ? toolPartDomId('result', part.toolUseId) : baseId,
      source: 'tool_results',
      producer,
      tokens: part.parts.reduce((sum, child) => sum + estimateMessagePartTokens(child, model), 0),
      hash: compositionBlockHash('tool_results', producer, title, text),
      title,
      detail: baseDetail,
      snippet: text,
    });
    return;
  }
  if (part.kind === 'tool_use') {
    const text = messagePartCopyText(part);
    const producer = part.name || 'unknown';
    const hashLabel = part.hashLabel || part.name || 'Tool use';
    const title = part.name || hashLabel;
    out.push({
      id: part.id ? toolPartDomId('use', part.id) : baseId,
      source: 'tool_calls',
      producer,
      tokens: estimateTokens(text),
      hash: compositionBlockHash('tool_calls', producer, hashLabel, part.hashText || text),
      title,
      detail: baseDetail,
      snippet: text,
    });
    return;
  }
  if (part.kind === 'image' || part.kind === 'image_url') {
    const text = messagePartSnippetText(part);
    const title = part.label;
    const hashLabel = part.hashLabel || (part.kind === 'image' ? 'Attachment' : 'Image');
    const source = role === 'assistant' ? 'assistant' : 'user';
    out.push({
      id: baseId,
      source,
      producer: 'attachments',
      tokens: estimateMessagePartTokens(part, model),
      hash: compositionBlockHash(source, 'attachments', hashLabel, part.hashText || ''),
      title,
      detail: baseDetail,
      snippet: text,
    });
    return;
  }
  if (part.kind === 'raw' && isReasoningLabel(part.label)) {
    const text = reasoningPartText(part);
    if (!text) return;
    const title = part.label;
    out.push({
      id: baseId,
      source: 'assistant',
      producer: 'reasoning',
      tokens: estimateTokens(text),
      hash: compositionBlockHash('assistant', 'reasoning', 'Reasoning', text),
      title,
      detail: baseDetail,
      snippet: text,
    });
    return;
  }
  if (part.kind === 'raw') {
    // Unrecognized block types land in the explicit "other" bucket.
    const title = partTitle(part);
    const text = compactJson(part.raw);
    out.push({
      id: baseId,
      source: 'other',
      tokens: estimateTokens(text),
      hash: compositionBlockHash('other', '', title, text),
      title,
      detail: baseDetail,
      snippet: messagePartCopyText(part),
    });
    return;
  }

  // Unknown roles mirror the backend's explicit residual, not a guessed bucket.
  let source =
    role === 'assistant' ? 'assistant'
    : role === 'system' || role === 'developer' ? 'system'
    : role === 'user' ? 'user'
    : 'other';
  let producer = source === 'system' ? 'base' : source === 'user' || source === 'assistant' ? 'text' : '';
  let title = partTitle(part);
  const text = messagePartCopyText(part);
  if (source === 'user' && part.kind === 'text') {
    // Whole-block <system-reminder> wrappers are harness-injected system
    // weight, mirroring the backend's userTextUnit.
    const reminder = reminderProducer(part.text);
    if (reminder) {
      source = 'system';
      producer = reminder;
      title = 'System reminder';
    }
  }
  out.push({
    id: baseId,
    source,
    producer: producer || undefined,
    tokens: estimateTokens(text),
    hash: compositionBlockHash(source, producer, title, text),
    title,
    detail: baseDetail,
    snippet: text,
  });
}

// Mirrors the backend's reminderProducer: a block that is wholly a
// <system-reminder> wrapper classifies as system, by content heuristic.
function reminderProducer(text: string): string | null {
  const t = text.trim();
  if (!t.startsWith('<system-reminder>') || !t.endsWith('</system-reminder>')) return null;
  if (t.includes('CLAUDE.md')) return 'claude_md';
  if (t.includes('MEMORY.md')) return 'memory';
  return 'reminder';
}

function toolUseNames(messages: PromptMessage[]): Map<string, string> {
  const out = new Map<string, string>();
  const visit = (part: MessagePart) => {
    if (part.kind === 'tool_use' && part.id) out.set(part.id, part.name);
    if (part.kind === 'tool_result') part.parts.forEach(visit);
  };
  messages.forEach((message) => message.parts.forEach(visit));
  return out;
}

function compositionBlockHash(source: string, producer: string, label: string, text: string): string {
  const encoder = new TextEncoder();
  let hash = 0xcbf29ce484222325n;
  const prime = 0x100000001b3n;
  const mask = 0xffffffffffffffffn;
  const bytes = [
    ...encoder.encode(source),
    0,
    ...encoder.encode(producer),
    0,
    ...encoder.encode(label),
    0,
    ...encoder.encode(text),
  ];
  for (const byte of bytes) {
    hash ^= BigInt(byte);
    hash = (hash * prime) & mask;
  }
  return hash.toString(16);
}

function partTitle(part: MessagePart): string {
  if (part.kind === 'text') return 'Text block';
  if (part.kind === 'empty') return part.label;
  if (part.kind === 'raw') return part.label;
  return messagePartCopyText(part).split('\n')[0] || 'Message block';
}

// isReasoningLabel names the block types that carry a model's own reasoning:
// Anthropic's thinking and redacted_thinking blocks, and the Responses API's
// top-level reasoning item.
function isReasoningLabel(label: string): boolean {
  return label.includes('thinking') || label === 'reasoning';
}

// reasoningPartText reads whichever field this vendor put the reasoning in.
// The three probes are not alternatives to pick between — a Responses reasoning
// item routinely arrives with `summary: []` and its whole text in
// `content: [{type:"reasoning_text", …}]`, so stopping at the summary returns
// the empty string for a block several thousand characters long.
function reasoningPartText(part: Extract<MessagePart, { kind: 'raw' }>): string {
  if (!isRecord(part.raw)) return '';
  if (typeof part.raw.thinking === 'string' && part.raw.thinking !== '') return part.raw.thinking;
  const summary = reasoningTexts(part.raw.summary);
  if (summary) return summary;
  return reasoningTexts(part.raw.content);
}

function reasoningTexts(value: unknown): string {
  if (!Array.isArray(value)) return '';
  return value
    .map((item) => (isRecord(item) && typeof item.text === 'string' ? item.text : ''))
    .filter(Boolean)
    .join('\n');
}

export function snippet(text: string): string {
  return text.replace(/\s+/g, ' ').trim().slice(0, 220);
}

function estimateMessagePartTokens(part: MessagePart, model: string): number {
  if (part.kind === 'text') return estimateTokens(part.text);
  if (part.kind === 'tool_use') return estimateTokens(messagePartCopyText(part));
  if (part.kind === 'tool_result') return part.parts.reduce((sum, child) => sum + estimateMessagePartTokens(child, model), 0);
  if (part.kind === 'image' || part.kind === 'image_url') {
    if (part.width && part.height) return imageTokensForModel(part.width, part.height, model, part.detail);
    if (part.visualTokens !== undefined) return part.visualTokens;
    return unknownSizeImageTokensForModel(model);
  }
  if (part.kind === 'empty') return estimateTokens(part.label);
  return estimateTokens(messagePartCopyText(part));
}

function estimateTokens(text: string): number {
  const normalized = text.trim();
  if (!normalized) return 0;
  return Math.max(1, Math.round(normalized.length / 4));
}

interface ImageMeta {
  width?: number;
  height?: number;
  visualTokens?: number;
}

interface ImageTier {
  maxLongEdge: number;
  maxTokens: number;
}

const STANDARD_IMAGE_TIER: ImageTier = { maxLongEdge: 1568, maxTokens: 1568 };
const HIGH_IMAGE_TIER: ImageTier = { maxLongEdge: 2576, maxTokens: 4784 };

function imageMeta(src: string): ImageMeta {
  const bytes = dataUrlBytes(src);
  if (!bytes) return {};
  const size = imageSize(bytes);
  if (!size) return {};
  return {
    width: size.width,
    height: size.height,
    visualTokens: visualTokensForSize(size.width, size.height, STANDARD_IMAGE_TIER),
  };
}

// DEFAULT_OPENAI_TILE mirrors the backend's fail-open tile spec for
// uncatalogued OpenAI models.
const DEFAULT_OPENAI_TILE: OpenAITileSpec = { base: 70, tile: 140 };

function looksOpenAIModel(model: string): boolean {
  const name = model.toLowerCase();
  return name.includes('gpt') || /^o\d/.test(name) || name.includes('codex');
}

function imageTokensForModel(width: number, height: number, model: string, detail?: string): number {
  const patch = openAIPatchSpec(model);
  if (patch) return Math.ceil(openAIPatchCount(width, height, patch.patchBudget) * patch.multiplier);
  const tile = openAITileSpec(model);
  if (tile) return openAITileTokens(width, height, tile, detail);
  if (looksOpenAIModel(model)) return openAITileTokens(width, height, DEFAULT_OPENAI_TILE, detail);
  return visualTokensForSize(width, height, claudeImageTier(model));
}

function unknownSizeImageTokensForModel(model: string): number {
  const tile = openAITileSpec(model);
  if (tile) return tile.base;
  if (openAIPatchSpec(model)) return 0;
  if (looksOpenAIModel(model)) return DEFAULT_OPENAI_TILE.base;
  return 0;
}

interface OpenAIPatchSpec {
  patchBudget: number;
  multiplier: number;
}

function openAIPatchSpec(model: string): OpenAIPatchSpec | null {
  const name = model.toLowerCase();
  if (name.includes('gpt-5.4-mini') || name.includes('gpt-5-mini') || name.includes('gpt-4.1-mini')) {
    return { patchBudget: 1536, multiplier: 1.62 };
  }
  if (name.includes('gpt-5.4-nano') || name.includes('gpt-5-nano') || name.includes('gpt-4.1-nano')) {
    return { patchBudget: 1536, multiplier: 2.46 };
  }
  if (name === 'o4-mini' || name.includes('o4-mini-')) return { patchBudget: 1536, multiplier: 1.72 };
  return null;
}

function openAIPatchCount(width: number, height: number, patchBudget: number): number {
  if (width <= 0 || height <= 0) return 0;
  let w = width;
  let h = height;
  const long = Math.max(w, h);
  if (long > 2048) {
    const scale = 2048 / long;
    w *= scale;
    h *= scale;
  }
  let patches = patch32Count(w, h);
  if (patches <= patchBudget) return patches;

  const shrink = Math.sqrt((32 * 32 * patchBudget) / (w * h));
  const adjusted = shrink * Math.min(
    Math.floor((w * shrink) / 32) / ((w * shrink) / 32),
    Math.floor((h * shrink) / 32) / ((h * shrink) / 32),
  );
  if (!Number.isFinite(adjusted) || adjusted <= 0) return patchBudget;
  patches = patch32Count(Math.floor(w * adjusted), Math.floor(h * adjusted));
  return Math.min(patches, patchBudget);
}

function patch32Count(width: number, height: number): number {
  if (width <= 0 || height <= 0) return 0;
  return Math.ceil(width / 32) * Math.ceil(height / 32);
}

interface OpenAITileSpec {
  base: number;
  tile: number;
}

function openAITileSpec(model: string): OpenAITileSpec | null {
  const name = model.toLowerCase();
  if (name === 'gpt-5' || name === 'gpt-5-chat-latest') return { base: 70, tile: 140 };
  if (name.includes('gpt-4o-mini')) return { base: 2833, tile: 5667 };
  if (name.includes('gpt-4o') || name.includes('gpt-4.1') || name.includes('gpt-4.5')) return { base: 85, tile: 170 };
  if (name === 'o1' || name.includes('o1-') || name === 'o1-pro' || name.includes('o1-pro-') || name === 'o3' || name.includes('o3-')) {
    return { base: 75, tile: 150 };
  }
  if (name.includes('computer-use-preview')) return { base: 65, tile: 129 };
  return null;
}

function openAITileTokens(width: number, height: number, spec: OpenAITileSpec, detail?: string): number {
  if (detail?.toLowerCase() === 'low') return spec.base;
  if (width <= 0 || height <= 0) return spec.base;
  let w = width;
  let h = height;
  const long = Math.max(w, h);
  if (long > 2048) {
    const scale = 2048 / long;
    w *= scale;
    h *= scale;
  }
  const short = Math.min(w, h);
  if (short > 0 && short !== 768) {
    const scale = 768 / short;
    w *= scale;
    h *= scale;
  }
  return spec.base + Math.ceil(w / 512) * Math.ceil(h / 512) * spec.tile;
}

function dataUrlBytes(src: string): Uint8Array | null {
  const match = /^data:[^;,]+;base64,(.*)$/s.exec(src);
  if (!match) return null;
  try {
    const binary = atob(match[1]);
    const bytes = new Uint8Array(binary.length);
    for (let i = 0; i < binary.length; i += 1) bytes[i] = binary.charCodeAt(i);
    return bytes;
  } catch {
    return null;
  }
}

function imageSize(bytes: Uint8Array): { width: number; height: number } | null {
  if (
    bytes.length >= 24 &&
    bytes[0] === 0x89 &&
    bytes[1] === 0x50 &&
    bytes[2] === 0x4e &&
    bytes[3] === 0x47
  ) {
    return { width: readU32BE(bytes, 16), height: readU32BE(bytes, 20) };
  }
  if (bytes.length >= 10 && bytes[0] === 0x47 && bytes[1] === 0x49 && bytes[2] === 0x46) {
    return { width: readU16LE(bytes, 6), height: readU16LE(bytes, 8) };
  }
  if (bytes.length >= 4 && bytes[0] === 0xff && bytes[1] === 0xd8) {
    return jpegSize(bytes);
  }
  return null;
}

function jpegSize(bytes: Uint8Array): { width: number; height: number } | null {
  let i = 2;
  while (i + 8 < bytes.length) {
    if (bytes[i] !== 0xff) {
      i += 1;
      continue;
    }
    while (i < bytes.length && bytes[i] === 0xff) i += 1;
    const marker = bytes[i];
    i += 1;
    if (marker === 0xd9 || marker === 0xda) return null;
    if (i + 1 >= bytes.length) return null;
    const length = readU16BE(bytes, i);
    if (length < 2 || i + length > bytes.length) return null;
    if (
      (marker >= 0xc0 && marker <= 0xc3) ||
      (marker >= 0xc5 && marker <= 0xc7) ||
      (marker >= 0xc9 && marker <= 0xcb) ||
      (marker >= 0xcd && marker <= 0xcf)
    ) {
      return { height: readU16BE(bytes, i + 3), width: readU16BE(bytes, i + 5) };
    }
    i += length;
  }
  return null;
}

function readU16BE(bytes: Uint8Array, offset: number): number {
  return (bytes[offset] << 8) | bytes[offset + 1];
}

function readU16LE(bytes: Uint8Array, offset: number): number {
  return bytes[offset] | (bytes[offset + 1] << 8);
}

function readU32BE(bytes: Uint8Array, offset: number): number {
  return ((bytes[offset] << 24) | (bytes[offset + 1] << 16) | (bytes[offset + 2] << 8) | bytes[offset + 3]) >>> 0;
}

function claudeImageTier(model: string): ImageTier {
  const name = model.toLowerCase();
  // Dash forms match real model ids (claude-opus-4-8); dot/space forms cover
  // display names. Mirrors the backend list.
  const highModels = [
    'fable-5', 'fable 5', 'mythos-5', 'mythos 5',
    'opus-4-8', 'opus-4.8', 'opus 4.8',
    'opus-4-7', 'opus-4.7', 'opus 4.7',
    'sonnet-5', 'sonnet 5',
  ];
  return highModels.some((high) => name.includes(high)) ? HIGH_IMAGE_TIER : STANDARD_IMAGE_TIER;
}

function visualTokensForSize(width: number, height: number, tier: ImageTier): number {
  if (width <= 0 || height <= 0) return 0;
  let w = width;
  let h = height;
  const long = Math.max(w, h);
  if (long > tier.maxLongEdge) {
    const scale = tier.maxLongEdge / long;
    w *= scale;
    h *= scale;
  }
  if (visualPatchCount(w, h) <= tier.maxTokens) return visualPatchCount(w, h);

  let lo = 0;
  let hi = 1;
  for (let i = 0; i < 64; i += 1) {
    const mid = (lo + hi) / 2;
    if (visualPatchCount(w * mid, h * mid) <= tier.maxTokens) lo = mid;
    else hi = mid;
  }
  return visualPatchCount(w * lo, h * lo);
}

function visualPatchCount(width: number, height: number): number {
  if (width <= 0 || height <= 0) return 0;
  return Math.ceil(width / 28) * Math.ceil(height / 28);
}

function toolBlockText(tool: ToolInfo): string {
  return [
    tool.type,
    tool.name,
    tool.description,
    prettyJson(tool.schema),
    ...tool.tools.map(toolBlockText),
  ].filter(Boolean).join('\n');
}

function promptMessage(raw: unknown): PromptMessage {
  if (typeof raw === 'string') return { role: 'user', parts: [{ kind: 'text', text: raw }] };
  if (!isRecord(raw)) return { role: 'unknown', parts: [{ kind: 'raw', label: 'value', raw: stripCacheControl(raw) }] };

  const type = typeof raw.type === 'string' ? raw.type : '';
  if (type === 'function_call') {
    return {
      role: 'assistant',
      parts: [functionCallPart(raw)],
    };
  }
  if (type === 'reasoning') {
    // A top-level Responses reasoning item declares no role. Falling through
    // would label the card "reasoning" as if that were a speaker; it is the
    // assistant thinking, so it renders as one reasoning part of an assistant
    // turn — the same shape Anthropic sends inline.
    return {
      role: 'assistant',
      parts: [{ kind: 'raw', label: 'reasoning', raw: stripCacheControl(raw) }],
    };
  }
  if (type === 'function_call_output') {
    return {
      role: 'tool',
      parts: [functionCallOutputPart(raw)],
    };
  }

  const role =
    typeof raw.role === 'string'
      ? raw.role
      : type === 'message'
        ? 'message'
        : type || 'unknown';
  if (role === 'tool' && typeof raw.tool_call_id === 'string') {
    return {
      role,
      parts: [{ kind: 'tool_result', toolUseId: raw.tool_call_id, parts: messageParts(raw.content) }],
    };
  }
  const parts = messageParts(raw.content);
  if (Array.isArray(raw.tool_calls)) {
    parts.push(...raw.tool_calls.map(functionCallPart));
  }
  if (parts.length === 0) parts.push({ kind: 'raw', label: type || role, raw: stripCacheControl(raw) });
  return { role, parts };
}

function messageParts(content: unknown): MessagePart[] {
  if (typeof content === 'string') return [{ kind: 'text', text: content }];
  if (Array.isArray(content)) return content.flatMap(messagePart);
  if (content == null) return [];
  return [messagePart(content)].flat();
}

function messagePart(raw: unknown): MessagePart | MessagePart[] {
  if (typeof raw === 'string') return { kind: 'text', text: raw };
  if (!isRecord(raw)) return { kind: 'raw', label: 'value', raw: stripCacheControl(raw) };

  const type = typeof raw.type === 'string' ? raw.type : '';
  if (type === 'tool_use' || type === 'server_tool_use' || type === 'mcp_tool_use') {
    const name = typeof raw.name === 'string' ? raw.name : '';
    return {
      kind: 'tool_use',
      id: typeof raw.id === 'string' ? raw.id : '',
      name,
      input: stripCacheControl(raw.input ?? {}),
      // Mirrors the backend block label: the verbatim tool name when present.
      hashLabel: name || 'Tool use',
      hashText: compactJson(raw),
    };
  }
  if (type === 'tool_result') {
    return {
      kind: 'tool_result',
      toolUseId: typeof raw.tool_use_id === 'string' ? raw.tool_use_id : '',
      parts: messageParts(raw.content),
    };
  }
  if (type === 'image') {
    const image = imagePart(raw);
    if (image) return image;
  }
  if (type === 'image_url') {
    const image = imageURLPart(raw);
    if (image) return image;
  }
  if (type === 'input_image') {
    const image = inputImagePart(raw);
    if (image) return image;
  }
  if (type === 'function_call') return functionCallPart(raw);
  if (type === 'function_call_output') return functionCallOutputPart(raw);

  const text = textField(raw);
  if (text !== null) return { kind: 'text', text };
  return { kind: 'raw', label: type || 'part', raw: stripCacheControl(raw) };
}

function functionCallPart(raw: unknown): MessagePart {
  const record = isRecord(raw) ? raw : {};
  const fn = isRecord(record.function) ? record.function : {};
  const name =
    typeof record.name === 'string'
      ? record.name
      : typeof fn.name === 'string'
        ? fn.name
        : '';
  const id =
    typeof record.id === 'string'
      ? record.id
      : typeof record.call_id === 'string'
        ? record.call_id
        : '';
  const input = record.arguments ?? fn.arguments ?? record.input ?? {};
  return {
    kind: 'tool_use',
    id,
    name,
    input: parseJsonMaybe(input),
    // Mirrors the backend block label: the verbatim tool name, falling back to
    // the item type for name-less Responses call items.
    hashLabel: name || (stringField(record.type) === 'function_call' ? 'function_call' : 'Tool use'),
    hashText: compactJson(record),
  };
}

function functionCallOutputPart(raw: unknown): MessagePart {
  const record = isRecord(raw) ? raw : {};
  return {
    kind: 'tool_result',
    toolUseId: typeof record.call_id === 'string' ? record.call_id : '',
    parts: messageParts(record.output),
  };
}

function imagePart(record: Record<string, unknown>): MessagePart | null {
  const source = isRecord(record.source) ? record.source : null;
  if (!source) return null;
  if (typeof source.url === 'string') {
    return { kind: 'image_url', url: source.url, label: 'Image', detail: stringField(source.detail), ...imageMeta(source.url) };
  }
  if (source.type === 'base64' && typeof source.data === 'string') {
    const mediaType = typeof source.media_type === 'string' ? source.media_type : 'image/jpeg';
    const src = `data:${mediaType};base64,${source.data}`;
    return { kind: 'image', src, label: mediaType, hashLabel: 'Attachment', hashText: '', ...imageMeta(src) };
  }
  return null;
}

function imageURLPart(record: Record<string, unknown>): MessagePart | null {
  const imageURL = record.image_url;
  if (typeof imageURL === 'string') {
    return { kind: 'image_url', url: imageURL, label: 'Image', detail: stringField(record.detail), ...imageMeta(imageURL) };
  }
  if (isRecord(imageURL) && typeof imageURL.url === 'string') {
    return {
      kind: 'image_url',
      url: imageURL.url,
      label: 'Image',
      detail: stringField(record.detail) || stringField(imageURL.detail),
      ...imageMeta(imageURL.url),
    };
  }
  return null;
}

function inputImagePart(record: Record<string, unknown>): MessagePart | null {
  if (typeof record.image_url === 'string') {
    return { kind: 'image_url', url: record.image_url, label: 'Image', detail: stringField(record.detail), ...imageMeta(record.image_url) };
  }
  if (isRecord(record.image_url) && typeof record.image_url.url === 'string') {
    return {
      kind: 'image_url',
      url: record.image_url.url,
      label: 'Image',
      detail: stringField(record.detail) || stringField(record.image_url.detail),
      ...imageMeta(record.image_url.url),
    };
  }
  return null;
}

function textField(record: Record<string, unknown>): string | null {
  if (typeof record.text === 'string') return record.text;
  if (typeof record.output_text === 'string') return record.output_text;
  if (typeof record.input_text === 'string') return record.input_text;
  return null;
}

function stringField(value: unknown): string | undefined {
  return typeof value === 'string' && value !== '' ? value : undefined;
}

function parseJsonMaybe(value: unknown): unknown {
  if (typeof value !== 'string') return stripCacheControl(value);
  try {
    return stripCacheControl(JSON.parse(value));
  } catch {
    return value;
  }
}

function systemBlockDomId(index: number): string {
  return `prompt-system-${index}`;
}

function toolBlockDomId(index: number): string {
  return `prompt-tool-${index}`;
}

function messagePartDomId(messageIndex: number, path: number[], scope = 'prompt'): string {
  return `${scope}-message-${messageIndex}-part-${path.join('-')}`;
}

function toolPartDomId(kind: 'use' | 'result', id: string): string {
  return `tool-${kind}-${id.replace(/[^A-Za-z0-9_-]/g, '_')}`;
}

export function jumpToPromptBlock(id: string): void {
  const el = document.getElementById(id);
  if (!el) return;
  revealDetails(el);
  highlightElement(el);
}

function jumpToToolPart(kind: 'use' | 'result', id: string): void {
  const el = document.getElementById(toolPartDomId(kind, id));
  if (!el) return;
  revealDetails(el);
  highlightElement(el);
}

function revealDetails(el: HTMLElement): void {
  let parent = el.parentElement;
  while (parent) {
    if (parent instanceof HTMLDetailsElement) parent.open = true;
    parent = parent.parentElement;
  }
}

function highlightElement(el: HTMLElement): void {
  el.scrollIntoView({ behavior: 'auto', block: 'start' });
  el.classList.remove(styles.messageJumpHighlight);
  window.requestAnimationFrame(() => {
    el.classList.add(styles.messageJumpHighlight);
    window.setTimeout(() => el.classList.remove(styles.messageJumpHighlight), 1800);
  });
}

function messagePartCopyText(part: MessagePart): string {
  if (part.kind === 'text') return part.text;
  if (part.kind === 'tool_use') {
    const name = part.name ? ` ${part.name}` : '';
    const id = part.id ? ` (${part.id})` : '';
    return `Tool Use${name}${id}\n${prettyJson(part.input)}`;
  }
  if (part.kind === 'tool_result') {
    const id = part.toolUseId ? ` ${part.toolUseId}` : '';
    return [`Tool Result${id}`, ...part.parts.map(messagePartCopyText)].join('\n');
  }
  if (part.kind === 'image') return `Image: ${part.label}\n${part.src}`;
  if (part.kind === 'image_url') return `Image: ${part.label}\n${part.url}`;
  if (part.kind === 'empty') return part.label;
  return `${part.label}\n${prettyJson(part.raw)}`;
}

function messagePartSnippetText(part: MessagePart): string {
  if (part.kind === 'image' || part.kind === 'image_url') {
    const dims = part.width && part.height ? `${part.width}x${part.height}` : 'unknown size';
    const detail = part.detail ? `, ${part.detail}` : '';
    return `Image: ${part.label} (${dims}${detail})`;
  }
  if (part.kind === 'tool_result') {
    const id = part.toolUseId ? ` ${part.toolUseId}` : '';
    return [`Tool Result${id}`, ...part.parts.map(messagePartSnippetText)].join('\n');
  }
  return messagePartCopyText(part);
}

function isSingleLineText(text: string): boolean {
  return !text.includes('\n') && text.length <= 160;
}

// systemBlocks mirrors the backend's systemUnits: one block for the plain
// string form, one per element for the structured (array) form — text blocks
// weigh their text, anything else its compact JSON.
function systemBlocks(system: unknown): string[] {
  if (typeof system === 'string') return [system];
  if (system == null) return [];
  if (Array.isArray(system)) {
    return system.map(systemBlockText).filter((s) => s !== '');
  }
  return [compactJson(system)];
}

// systemBlockText reads a block's text through the same field probe the message
// renderer uses, so a preamble the backend lifted out of a Responses `input`
// array — whose blocks are typed `input_text`, not `text` — reads as prose
// rather than as a JSON dump.
function systemBlockText(block: unknown): string {
  if (typeof block === 'string') return block;
  if (!isRecord(block)) return compactJson(block);
  return textField(block) || compactJson(block);
}

function toolInfos(tools: unknown): ToolInfo[] {
  if (!Array.isArray(tools)) return [];
  return tools.map(toolInfo);
}

function toolInfo(tool: unknown): ToolInfo {
  const record = isRecord(tool) ? tool : {};
  const fn = isRecord(record.function) ? record.function : {};
  const type = typeof record.type === 'string' ? record.type : 'function';
  const schema = record.input_schema ?? record.parameters ?? fn.parameters;
  const nestedTools = Array.isArray(record.tools) ? record.tools.map(toolInfo) : [];
  return {
    type,
    name:
      typeof record.name === 'string'
        ? record.name
        : typeof fn.name === 'string'
          ? fn.name
          : type,
    description:
      typeof record.description === 'string'
        ? record.description
        : typeof fn.description === 'string'
          ? fn.description
          : '',
    propertyCount: schemaPropertyCount(schema),
    schema: stripCacheControl(schema ?? {}),
    tools: nestedTools,
    hashText: compactJson(tool),
  };
}

function SchemaView({ schema }: { schema: unknown }) {
  const fields = schemaFields(schema);
  if (fields.length === 0) {
    return (
      <div className={styles.schemaEmpty}>
        <span>No structured inputs.</span>
        {isRecord(schema) && Object.keys(schema).length > 0 ? (
          <details className={styles.schemaRaw}>
            <summary>Raw schema</summary>
            <pre className={styles.schemaCode}>{prettyJson(schema)}</pre>
          </details>
        ) : null}
      </div>
    );
  }
  return (
    <div className={styles.schemaFields}>
      {fields.map((field) => (
        <SchemaField key={field.path} field={field} />
      ))}
    </div>
  );
}

interface SchemaFieldInfo {
  path: string;
  name: string;
  type: string;
  description: string;
  required: boolean;
  enumValues: string[];
  children: SchemaFieldInfo[];
}

function SchemaField({ field }: { field: SchemaFieldInfo }) {
  return (
    <div className={styles.schemaField}>
      <div className={styles.schemaFieldTop}>
        <span className={styles.schemaFieldName}>{field.name}</span>
        <span className={styles.schemaFieldType}>{field.type}</span>
        <span className={field.required ? styles.schemaRequired : styles.schemaOptional}>
          {field.required ? 'Required' : 'Optional'}
        </span>
      </div>
      {field.description ? <div className={styles.schemaFieldDesc}>{field.description}</div> : null}
      {field.enumValues.length > 0 ? (
        <div className={styles.schemaEnums}>
          {field.enumValues.map((value) => (
            <span key={value} className={styles.schemaEnum}>{value}</span>
          ))}
        </div>
      ) : null}
      {field.children.length > 0 ? (
        <div className={styles.schemaChildren}>
          {field.children.map((child) => (
            <SchemaField key={child.path} field={child} />
          ))}
        </div>
      ) : null}
    </div>
  );
}

function schemaFields(schema: unknown): SchemaFieldInfo[] {
  if (!isRecord(schema) || !isRecord(schema.properties)) return [];
  const required = stringSet(schema.required);
  return Object.entries(schema.properties).map(([name, value]) =>
    schemaFieldInfo(name, value, required.has(name), name),
  );
}

function schemaFieldInfo(name: string, schema: unknown, required: boolean, path: string): SchemaFieldInfo {
  const record = isRecord(schema) ? schema : {};
  const type = schemaType(record);
  const childRequired = stringSet(record.required);
  const childProperties = isRecord(record.properties)
    ? Object.entries(record.properties).map(([childName, childSchema]) =>
        schemaFieldInfo(childName, childSchema, childRequired.has(childName), `${path}.${childName}`),
      )
    : arrayItemFields(record, path);

  return {
    path,
    name,
    type,
    description: typeof record.description === 'string' ? record.description : '',
    required,
    enumValues: enumValues(record.enum),
    children: childProperties,
  };
}

function arrayItemFields(schema: Record<string, unknown>, path: string): SchemaFieldInfo[] {
  const items = schema.items;
  if (!isRecord(items) || !isRecord(items.properties)) return [];
  const required = stringSet(items.required);
  return Object.entries(items.properties).map(([name, value]) =>
    schemaFieldInfo(name, value, required.has(name), `${path}[].${name}`),
  );
}

function schemaType(schema: Record<string, unknown>): string {
  if (typeof schema.type === 'string') {
    if (schema.type === 'array' && isRecord(schema.items)) return `array<${schemaType(schema.items)}>`;
    return schema.type;
  }
  if (Array.isArray(schema.type)) return schema.type.filter((item) => typeof item === 'string').join(' | ') || 'value';
  if (Array.isArray(schema.enum)) return 'enum';
  if (Array.isArray(schema.anyOf)) return 'anyOf';
  if (Array.isArray(schema.oneOf)) return 'oneOf';
  return 'value';
}

function stringSet(value: unknown): Set<string> {
  if (!Array.isArray(value)) return new Set();
  return new Set(value.filter((item): item is string => typeof item === 'string'));
}

function enumValues(value: unknown): string[] {
  if (!Array.isArray(value)) return [];
  return value.map((item) => String(item)).slice(0, 12);
}

function schemaPropertyCount(schema: unknown): number {
  if (!isRecord(schema) || !isRecord(schema.properties)) return 0;
  return Object.keys(schema.properties).length;
}

function prettyJson(value: unknown): string {
  return JSON.stringify(value, null, 2);
}

function compactJson(value: unknown): string {
  return JSON.stringify(value);
}

function stripCacheControl(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(stripCacheControl);
  if (!isRecord(value)) return value;
  const out: Record<string, unknown> = {};
  for (const [key, child] of Object.entries(value)) {
    if (key !== 'cache_control') out[key] = stripCacheControl(child);
  }
  return out;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}
