# URnetwork SDK-JS

## Installation
```bash
npm install @urnetwork/sdk-js
```

## Deployment
- In sdk/build, run `make build_js`. This will generate all the types we need and build sdk/sdk-js/dist.
- Make sure the new version in package.json is bumped and everything is committed and pushed to the main branch.
- If you're not already, login to npm with `npm login`.
- Run `npm pack --dry-run` to see what will be included in the package.
- If everything looks good, run `npm publish`.
