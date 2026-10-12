// ESLint flat config for the license portal, adapted from web/eslint.config.js:
// typescript-eslint (type-aware, through the solution tsconfig.json) and the
// React hooks rules, zero warnings allowed (`bun run lint`). The UI keeps
// the dashboard's form-control rules; the server and the scripts get Node
// and Bun globals.
import js from '@eslint/js';
import reactHooks from 'eslint-plugin-react-hooks';
import globals from 'globals';
import tseslint from 'typescript-eslint';

export default tseslint.config(
  {
    // Generated: the `astryx theme build` outputs (plus the theme's static
    // descriptor, read by the Astryx CLI only), build output and reports.
    ignores: [
      'dist/**',
      'coverage/**',
      'node_modules/**',
      'ui/src/themes/*/*.js',
      'ui/src/themes/*/*.d.ts',
      'ui/src/themes/*/*.doc.mjs',
      'playwright-report/**',
      'test-results/**',
    ],
  },
  js.configs.recommended,
  ...tseslint.configs.recommendedTypeChecked,
  {
    languageOptions: {
      ecmaVersion: 2023,
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
      '@typescript-eslint/no-explicit-any': 'error',
      'no-console': ['error', { allow: ['error', 'warn'] }],
    },
  },
  {
    // The UI runs in the browser.
    files: ['ui/**'],
    languageOptions: { globals: { ...globals.browser } },
  },
  {
    // The server, the shared wire code, the build scripts, the e2e suite and
    // the root configs run under Bun or Node.
    files: ['server/**', 'shared/**', 'scripts/**', 'e2e/**', 'vite.config.ts', 'playwright.config.ts', 'eslint.config.js'],
    languageOptions: { globals: { ...globals.node, Bun: 'readonly' } },
  },
  {
    // Every form control is an Astryx component (docs/m3/ui.md): no raw
    // <select>/<input>/<textarea> and no .k-native-* styling outside the
    // field wrappers in ui/src/ui/fields/. A justified exception carries
    // a disable comment for the next line saying why.
    files: ['ui/src/**/*.tsx'],
    ignores: ['ui/src/**/*.test.tsx', 'ui/src/test/**', 'ui/src/ui/fields/**'],
    rules: {
      'no-restricted-syntax': [
        'error',
        {
          selector: 'JSXOpeningElement[name.name=/^(select|input|textarea)$/]',
          message: 'Use an Astryx control (ui/src/ui/fields: SelectField, TextField, NumberField, ...), not a raw form element.',
        },
        {
          // Astryx renders `description` between the label and the control;
          // help goes under it (the wrappers' `help`).
          selector:
            'JSXOpeningElement[name.name=/^(TextInput|TextArea|Selector|NumberInput|MultiSelector|Typeahead|Tokenizer|DateInput|DateTimeInput|TimeInput|InputGroup|CheckboxList|RadioList|SegmentedControl)$/] > JSXAttribute[name.name="description"]',
          message: 'Help goes under the control: use the ui/src/ui/fields wrapper and its `help` prop instead of `description`.',
        },
        {
          selector: 'JSXAttribute[name.name="className"] Literal[value=/\\bk-native-/]',
          message: 'The .k-native-* styles are gone; use an Astryx control.',
        },
      ],
    },
  },
  {
    // Tests, the scripts, the e2e suite and the configs may print and may
    // poke at untyped JSON.
    files: ['**/*.test.{ts,tsx}', 'server/test/**', 'ui/src/test/**', 'scripts/**', 'e2e/**', 'vite.config.ts', 'playwright.config.ts', 'eslint.config.js'],
    languageOptions: { globals: { ...globals.node, ...globals.browser } },
    rules: {
      '@typescript-eslint/no-unsafe-assignment': 'off',
      '@typescript-eslint/no-unsafe-member-access': 'off',
      '@typescript-eslint/no-non-null-assertion': 'off',
      // bun-types declares expect().rejects/resolves matchers as void, but
      // they return promises that must be awaited.
      '@typescript-eslint/await-thenable': 'off',
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
