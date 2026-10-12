// Module declarations for imports Bun loads as text (`with { type: 'text' }`).

declare module '*.sql' {
  const text: string;
  export default text;
}
