Generate a compact map of the repository showing files and their key symbols (functions, contracts, classes, types, events, etc.).

The map is ranked by recency and symbol visibility so the most important files appear first. Use this tool to quickly orient yourself in an unfamiliar codebase before diving into specific files.

**Parameters**
- `path` (optional): Root directory to map. Defaults to the working directory.
- `focus` (optional): Only include files whose path contains this substring. Examples: `"contracts/"`, `".sol"`, `"src/"`.
- `max_symbols` (optional): Maximum symbols to show per file. Default is 20.

**When to use**
- Starting a new audit or investigation to understand the overall structure.
- Identifying all contracts / entry points in a Solidity repo.
- Finding where key functions or types are defined across a large codebase.
- Scoping the blast radius before making a refactor.

**Output format**
```
## contracts/
  Token.sol
    contract Token
    function transfer
    function approve
    event Transfer
    event Approval
    error InsufficientBalance
```
