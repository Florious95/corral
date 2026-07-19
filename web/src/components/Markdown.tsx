import { Fragment, isValidElement, type ReactNode } from 'react';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';

function highlightedLine(line: string): ReactNode[] {
  const token = /("(?:\\.|[^"])*"|'(?:\\.|[^'])*'|\/\/.*$|\b(?:async|await|class|const|else|false|func|function|if|let|null|return|struct|true|var)\b|\b\d+(?:\.\d+)?\b)/g;
  return line.split(token).filter(Boolean).map((part, index) => {
    let className: string | undefined;
    if (/^['"]/.test(part)) className = 'code-string';
    else if (part.startsWith('//')) className = 'code-comment';
    else if (/^\d/.test(part)) className = 'code-number';
    else if (/^[a-z]+$/.test(part)) className = 'code-keyword';
    return <span key={index} className={className}>{part}</span>;
  });
}

function HighlightedCode({ className, children }: { className?: string; children?: ReactNode }) {
  const code = String(children).replace(/\n$/, '');
  if (!className?.startsWith('language-')) return <code>{children}</code>;
  const lines = code.split('\n');
  return (
    <code className={className}>{lines.map((line, index) => (
      <Fragment key={index}>{highlightedLine(line)}{index < lines.length - 1 && '\n'}</Fragment>
    ))}</code>
  );
}

export function Markdown({ text }: { text: string }) {
  return (
    <div className="markdown">
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        components={{
          a: ({ children, ...props }) => <a {...props} target="_blank" rel="noreferrer">{children}</a>,
          code: ({ className, children }) => <HighlightedCode className={className}>{children}</HighlightedCode>,
          pre: ({ children }) => {
            const className = isValidElement<{ className?: string }>(children) ? children.props.className : undefined;
            return <pre data-language={className?.replace(/^language-/, '') || 'text'}>{children}</pre>;
          },
        }}
      >
        {text}
      </ReactMarkdown>
    </div>
  );
}
