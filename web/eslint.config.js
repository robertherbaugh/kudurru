// ESLint flat config: typescript-eslint (type-aware) and the React hooks
// rules, zero warnings allowed (`bun run lint`).
import js from '@eslint/js';
import reactHooks from 'eslint-plugin-react-hooks';
import globals from 'globals';
import tseslint from 'typescript-eslint';

export default tseslint.config(
  { ignores: ['dist/**', 'coverage/**', 'mock-public/**', 'node_modules/**', 'src/api/generated.ts', 'playwright-report/**', 'test-results/**'] },
  js.configs.recommended,
  ...tseslint.configs.recommendedTypeChecked,
  {
    languageOptions: {
      ecmaVersion: 2023,
      globals: { ...globals.browser },
      parserOptions: { projectService: { allowDefaultProject: ['eslint.config.js'] }, tsconfigRootDir: import.meta.dirname },
    },
    plugins: { 'react-hooks': reactHooks },
    rules: {
      'react-hooks/rules-of-hooks': 'error',
      'react-hooks/exhaustive-deps': 'warn',
      '@typescript-eslint/consistent-type-imports': ['error', { fixStyle: 'inline-type-imports' }],
      '@typescript-eslint/no-unused-vars': ['error', { argsIgnorePattern: '^_', varsIgnorePattern: '^_' }],
      '@typescript-eslint/no-floating-promises': 'error',
      '@typescript-eslint/no-misused-promises': ['error', { checksVoidReturn: { attributes: false } }],
      'no-console': ['error', { allow: ['error', 'warn'] }],
    },
  },
  {
    files: ['**/*.test.{ts,tsx}', 'src/test/**', 'src/mocks/**', 'scripts/**', 'e2e/**', 'vite.config.ts', 'playwright.config.ts', 'eslint.config.js'],
    languageOptions: { globals: { ...globals.node, ...globals.browser } },
    rules: {
      '@typescript-eslint/no-unsafe-assignment': 'off',
      '@typescript-eslint/no-unsafe-member-access': 'off',
      '@typescript-eslint/no-non-null-assertion': 'off',
      'no-console': 'off',
    },
  },
  {
    // Playwright fixtures: `use` is not a React hook, and `{}` is the
    // fixture-without-dependencies idiom.
    files: ['e2e/**'],
    rules: {
      'react-hooks/rules-of-hooks': 'off',
      'no-empty-pattern': 'off',
    },
  },
);
