# URnetwork SDK-JS

## Installation
```bash
npm install @urnetwork/sdk-js
```

## Deployment
- In sdk/build, run `make build_js`. This will generate all the types we need and build sdk/sdk-js/dist.
- Make sure everything is committed and pushed to the main branch.
- If you're not already, login to npm with `npm login`.
- Run `npm pack --dry-run` to see what will be included in the package.
- If everything looks good, run `npm run release:patch` to publish the package. There are also `release:beta`, `release:minor`, and `release:major` scripts available for versioning.
- Tag it on Github after publishing like `git tag vx.y.z` and `git push --tags`.
