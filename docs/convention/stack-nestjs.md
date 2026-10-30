# NestJS — Architecture & Style Conventions

Layout, naming, module kinds, configuration, database access, validation, and test placement for NestJS projects.

Checked against NestJS 11.2.7, `@nestjs/cli` 11.0.24, `@nestjs/config` 4.0.4, `@nestjs/typeorm` 11.0.3, `@nestjs/mongoose` 11.0.4, TypeORM 0.3.31, and Mongoose 8.24.4. A claim below that names no version holds for these.

## Contents
- 0. Folder & file naming — strict
- 1. Project layout — feature modules
- 2. Module kinds
- 3. Configuration
- 4. Database
- 5. Validation
- 6. Testing — placement and Nest setup
- 7. CLI conventions
- 8. Strict-mode TypeScript

## 0. Folder & file naming — strict

Banned at any level: `*.utils.ts`, `*.helpers.ts`, `*.shared.ts`.

**Mandatory file suffixes** (matches `nest g`):

| Suffix | Construct | Example |
|---|---|---|
| `*.module.ts` | `@Module()` | `users.module.ts` |
| `*.controller.ts` | `@Controller(path)` | `users.controller.ts` |
| `*.service.ts` | `@Injectable()` business logic | `users.service.ts` |
| `*.guard.ts` | `implements CanActivate` | `roles.guard.ts` |
| `*.interceptor.ts` | `implements NestInterceptor` | `logging.interceptor.ts` |
| `*.pipe.ts` | `implements PipeTransform` | `validation.pipe.ts` |
| `*.filter.ts` | `implements ExceptionFilter` | `http-exception.filter.ts` |
| `*.middleware.ts` | `implements NestMiddleware` | `auth.middleware.ts` |
| `*.decorator.ts` | parameter/method decorator | `current-user.decorator.ts` |
| `*.gateway.ts` | WebSocket gateway | `events.gateway.ts` |
| `*.dto.ts` | DTO with `class-validator` | `create-user.dto.ts` |
| `*.entity.ts` | TypeORM `@Entity()` | `user.entity.ts` |
| `*.schema.ts` | Mongoose `@Schema()` | `cat.schema.ts` |
| `*.spec.ts` | co-located unit test | `users.service.spec.ts` |
| `*.e2e-spec.ts` | e2e (in `test/`) | `users.e2e-spec.ts` |

Class names match files: `users.service.ts` → `UsersService`. IDE
navigation depends on it.

The table gives the **filename suffix** only. §1 gives the folder for
the files it lists (e.g. `*.entity.ts` goes in `entities/`); it gives
none for a guard, interceptor, pipe, filter, middleware, or gateway,
or for a decorator outside `src/auth/`. Where those live is the
project's to decide.

## 1. Project layout — feature modules

At the repo root:

- `.env`, `.env.example` — environment files, NEVER inside `src/`.
- `nest-cli.json`, `package.json`, `tsconfig.json`,
  `tsconfig.build.json`, `eslint.config.mjs` — tooling config.

Under `src/`:

- `src/main.ts` — bootstrap only. Global pipes and filters are bound
  as providers in `AppModule` (§5), not here.
- `src/app.module.ts` — root module; imports `CoreModule` and every
  feature module.
- `src/config/` — `@nestjs/config` wrappers: `configuration.ts`,
  `env.validation.ts`, and one file per namespace
  (`database.config.ts`, `app.config.ts`).
- `src/database/` — `database.module.ts` (ORM registration),
  `data-source.ts` (the CLI data source migrations run against), and
  `migrations/`. Name the folder for the ORM when that reads better
  (`prisma/`, `orm/`, `mongo/`).
- `src/auth/` — a cross-cutting concern that owns providers is a
  shared **module**: `auth.module.ts`, `auth.service.ts`,
  `strategies/`, `decorators/`.
- `src/<feature>/` — one folder per bounded context, holding
  `<feature>.module.ts`, `<feature>.controller.ts`,
  `<feature>.service.ts`, the co-located `*.spec.ts` files, `dto/`
  (`create-`, `update-`, `query-<feature>.dto.ts`), and `entities/`
  (or `schemas/` under Mongoose).

Under `test/` — e2e only:

- `test/jest-e2e.json` — the e2e Jest config.
- `test/<feature>/<feature>.e2e-spec.ts` — one file per feature.

### Feature file responsibilities

| File | Owns |
|---|---|
| `<feature>.module.ts` | `@Module()`: imports deps, declares controllers/providers, exports public services. |
| `<feature>.controller.ts` | **Thin.** Parse, call service, return. No ORM access. |
| `<feature>.service.ts` | Business logic, transactions, cross-aggregate calls. |
| `dto/create-<feature>.dto.ts` | Input validation shape (class). |
| `dto/update-<feature>.dto.ts` | `extends PartialType(CreateXxxDto)`. |
| `dto/query-<feature>.dto.ts` | Pagination / filters. |
| `entities/<feature>.entity.ts` | TypeORM `@Entity()` (or `schemas/<feature>.schema.ts` for Mongoose). |

## 2. Module kinds

| Kind | Example | Notes |
|---|---|---|
| Root | `AppModule` | One per project. Imports `ConfigModule.forRoot({ isGlobal: true })`, `DatabaseModule`, every feature. |
| Feature | `UsersModule` | Each owns a folder; nothing leaks across without `exports`. |
| Shared | `DatabaseModule`, `AuthModule` | Imported once into root; re-exported services via `forFeature()`. |
| Dynamic | `ConfigModule`, `TypeOrmModule` | `forRoot()` once in root; `forFeature()` per feature. |

Avoid `@Global()` unless the provider is genuinely used everywhere
(logger, request context).

## 3. Configuration

- Configuration is read through `@nestjs/config` wrappers in
  `src/config/`, registered once in the root module with
  `ConfigModule.forRoot({ isGlobal: true, load: [...], validate })`.
- Each config file exports a namespace built with `registerAs`, so
  consumers read `database.host` rather than a flat key.
- `src/config/env.validation.ts` exports the `validate` function
  `ConfigModule` calls: it builds a `class-validator`-decorated
  `EnvironmentVariables` class from the raw environment and throws on
  the first invalid value, so a misconfigured process fails at boot
  instead of at first use.

## 4. Database

### TypeORM (default)

- Register with `TypeOrmModule.forRootAsync`, injecting
  `ConfigService` — connection settings come from configuration, never
  from literals in the module.
- `autoLoadEntities: true`, so a feature's entities arrive with its
  `forFeature()` registration.
- **`synchronize: false`** — never `true` in production; it drops data
  on a schema change. Migrations in `src/database/migrations/` are the
  only schema history, applied with `migrationsRun`.
- Read single rows with the 0.3+ finders, `findOneBy` /
  `findOneByOrFail`. `findOneByOrFail` throws `EntityNotFoundError`;
  a project using `findOneByOrFail` writes an exception filter that
  maps that error to `NotFoundException`, in the folder where the
  project keeps its filters.

### Mongoose

- Schemas are `@Schema()`-decorated classes in
  `schemas/<feature>.schema.ts`, exported through
  `SchemaFactory.createForClass(...)`, with the document type declared
  as `HydratedDocument<T>`.
- Inject models with `@InjectModel(Entity.name)` and create documents
  with `Model.create`.

### Prisma

Prisma is wired by hand rather than through a Nest package:

- `src/prisma/prisma.module.ts` — a `@Global()` module exporting the
  service.
- `src/prisma/prisma.service.ts` — `PrismaService extends
  PrismaClient`, connecting in `OnModuleInit` and disconnecting in
  `OnModuleDestroy`.

Features inject `PrismaService` directly.

## 5. Validation

- `ValidationPipe` is bound globally through an `APP_PIPE` provider
  in `AppModule` — never `app.useGlobalPipes()` in `main.ts`, which
  binds no provider and so cannot be overridden in a test (§6).
  Configure it with `whitelist: true` (strip properties no DTO
  declares), `forbidNonWhitelisted: true` (reject a request that
  carries extras) and `transform: true` (cast query and path params
  to their declared types).
- DTOs are **classes**, not interfaces: TS interfaces are erased at
  compile time, and `ValidationPipe` reads the class metadata at
  runtime.
- **Never** `import type { CreateUserDto }` — always
  `import { CreateUserDto }`. A type-only import erases the same
  metadata.
- Derive related DTOs with `PartialType` / `PickType` / `OmitType` /
  `IntersectionType` from `@nestjs/mapped-types`.

## 6. Testing — placement and Nest setup

| Track | Pattern | Location | Runner |
|---|---|---|---|
| Unit | `*.spec.ts` | **co-located** with source | `jest` |
| Integration | `*.integration.spec.ts` | **co-located** in `src/<feature>/` | `jest` |
| E2E | `*.e2e-spec.ts` | `test/` folder | `jest --config ./test/jest-e2e.json` |

- Test names: `describe(...) > it(...)`.
- Anything that touches DI is assembled with
  `Test.createTestingModule(...)`.
- The in-process integration test client is `supertest` against the
  `INestApplication`.
- Mock a repository through its injection token,
  `getRepositoryToken(Entity)` — reach for `useMocker()` only when the
  dependency graph is too large to enumerate, and then only at module
  boundaries.
- A globally registered enhancer — `APP_PIPE`, `APP_GUARD`,
  `APP_INTERCEPTOR`, `APP_FILTER` — cannot be swapped in a test
  unless it is bound with `useExisting` instead of `useClass`; bind
  it that way so the provider behind it can be overridden.
- E2E exercises the built app — `nest build`, then `node dist/main`
  (the scaffold's `start:prod`) — with `@testcontainers/postgresql` /
  `@testcontainers/mongodb` supplying the services it talks to. Not
  `nest start`: it compiles the source again before it runs.


## 7. CLI conventions

- Scaffold with `nest g <kind> <name>`; it writes into `src/<name>/`,
  and `--flat` skips the folder. `nest g resource <name>` scaffolds
  the whole feature — module, controller, service, DTOs, entity.
- Always use the CLI's suffixes — tooling and IDE navigation rely on
  them.

## 8. Strict-mode TypeScript

Scaffold new projects with `nest new --strict`; that flag is how this
stack turns strict compilation on.

