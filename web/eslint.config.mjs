import js from '@eslint/js'
import nextPlugin from '@next/eslint-plugin-next'
import tseslint from 'typescript-eslint'

/**
 * Flat ESLint config.
 *
 * Built from the individual plugins rather than `eslint-config-next`, because
 * that package relies on @rushstack/eslint-patch, which fails to load under
 * ESLint 9's flat config. Wiring the plugins directly gives the same rule set
 * without the incompatible loader.
 */
export default tseslint.config(
  {
    ignores: ['.next/**', 'node_modules/**', 'coverage/**'],
  },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  {
    plugins: { '@next/next': nextPlugin },
    rules: {
      ...nextPlugin.configs.recommended.rules,
      ...nextPlugin.configs['core-web-vitals'].rules,
    },
  },
  {
    rules: {
      '@typescript-eslint/no-unused-vars': [
        'error',
        // A leading underscore is the convention for an intentionally
        // discarded value, notably the useEffect cleanup pattern.
        { argsIgnorePattern: '^_', varsIgnorePattern: '^_' },
      ],
      '@typescript-eslint/consistent-type-imports': [
        'error',
        { prefer: 'type-imports', fixStyle: 'inline-type-imports' },
      ],
      // The codebase targets `noUncheckedIndexedAccess`, so an unchecked
      // array access is a genuine error rather than a style preference.
      eqeqeq: ['error', 'always'],
    },
  },
  {
    // Test files legitimately construct loosely typed mock responses.
    files: ['**/*.test.ts', '**/*.test.tsx', 'src/test/**'],
    rules: {
      '@typescript-eslint/no-explicit-any': 'off',
      '@typescript-eslint/no-unsafe-assignment': 'off',
      '@typescript-eslint/no-unsafe-member-access': 'off',
      '@typescript-eslint/no-unsafe-call': 'off',
      '@typescript-eslint/no-unsafe-return': 'off',
    },
  },
)