import { defineConfig } from 'vitest/config'

// The tests here cover the part of this app that can be wrong without anyone
// noticing: cookie sealing, which credential a call is made with, how an upstream
// envelope is read, and how a number is rendered. All of it runs in Node, none of it
// needs a browser, and none of it needs either Go service to be up.
//
// There is deliberately no component-rendering suite. What a table looks like is
// checked by looking at it; what "isReadable: false" renders as is not, and that is
// what these tests are for.
export default defineConfig({
  test: {
    environment: 'node',
    include: ['lib/**/*.test.ts'],
  },
})
